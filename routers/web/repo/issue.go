// Copyright 2014 The Gogs Authors. All rights reserved.
// Copyright 2018 The Gitea Authors. All rights reserved.
// Use of this source code is governed by a MIT-style
// license that can be found in the LICENSE file.

package repo

import (
	"bytes"
	stdCtx "context"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"code.gitea.io/gitea/models"
	"code.gitea.io/gitea/models/db"
	issues_model "code.gitea.io/gitea/models/issues"
	"code.gitea.io/gitea/models/organization"
	project_model "code.gitea.io/gitea/models/project"
	pull_model "code.gitea.io/gitea/models/pull"
	repo_model "code.gitea.io/gitea/models/repo"
	"code.gitea.io/gitea/models/unit"
	user_model "code.gitea.io/gitea/models/user"
	"code.gitea.io/gitea/modules/base"
	"code.gitea.io/gitea/modules/context"
	"code.gitea.io/gitea/modules/convert"
	"code.gitea.io/gitea/modules/git"
	issue_indexer "code.gitea.io/gitea/modules/indexer/issues"
	"code.gitea.io/gitea/modules/log"
	"code.gitea.io/gitea/modules/markup"
	"code.gitea.io/gitea/modules/markup/markdown"
	"code.gitea.io/gitea/modules/setting"
	api "code.gitea.io/gitea/modules/structs"
	"code.gitea.io/gitea/modules/templates/vars"
	"code.gitea.io/gitea/modules/timeutil"
	"code.gitea.io/gitea/modules/upload"
	"code.gitea.io/gitea/modules/util"
	"code.gitea.io/gitea/modules/web"
	asymkey_service "code.gitea.io/gitea/services/asymkey"
	comment_service "code.gitea.io/gitea/services/comments"
	"code.gitea.io/gitea/services/forms"
	issue_service "code.gitea.io/gitea/services/issue"
	pull_service "code.gitea.io/gitea/services/pull"
)

const (
	tplAttachment base.TplName = "repo/issue/view_content/attachments"

	tplIssues      base.TplName = "repo/issue/list"
	tplIssueNew    base.TplName = "repo/issue/new"
	tplIssueChoose base.TplName = "repo/issue/choose"
	tplIssueView   base.TplName = "repo/issue/view"

	tplReactions base.TplName = "repo/issue/view_content/reactions"

	issueTemplateKey      = "IssueTemplate"
	issueTemplateTitleKey = "IssueTemplateTitle"
)

// IssueTemplateCandidates issue templates
var IssueTemplateCandidates = []string{
	"ISSUE_TEMPLATE.md",
	"issue_template.md",
	".gitea/ISSUE_TEMPLATE.md",
	".gitea/issue_template.md",
	".github/ISSUE_TEMPLATE.md",
	".github/issue_template.md",
}

// MustAllowUserComment checks to make sure if an issue is locked.
// If locked and user has permissions to write to the repository,
// then the comment is allowed, else it is blocked
func MustAllowUserComment(ctx *context.Context) {
	issue := GetActionIssue(ctx)
	if ctx.Written() {
		return
	}

	if issue.IsLocked && !ctx.Repo.CanWriteIssuesOrPulls(issue.IsPull) && !ctx.Doer.IsAdmin {
		ctx.Flash.Error(ctx.Tr("repo.issues.comment_on_locked"))
		ctx.Redirect(issue.HTMLURL())
		return
	}
}

// MustEnableIssues check if repository enable internal issues
func MustEnableIssues(ctx *context.Context) {
	if !ctx.Repo.CanRead(unit.TypeIssues) &&
		!ctx.Repo.CanRead(unit.TypeExternalTracker) {
		ctx.NotFound("MustEnableIssues", nil)
		return
	}

	unit, err := ctx.Repo.Repository.GetUnit(unit.TypeExternalTracker)
	if err == nil {
		ctx.Redirect(unit.ExternalTrackerConfig().ExternalTrackerURL)
		return
	}
}

// MustAllowPulls check if repository enable pull requests and user have right to do that
func MustAllowPulls(ctx *context.Context) {
	if !ctx.Repo.Repository.CanEnablePulls() || !ctx.Repo.CanRead(unit.TypePullRequests) {
		ctx.NotFound("MustAllowPulls", nil)
		return
	}

	// User can send pull request if owns a forked repository.
	if ctx.IsSigned && repo_model.HasForkedRepo(ctx.Doer.ID, ctx.Repo.Repository.ID) {
		ctx.Repo.PullRequest.Allowed = true
		ctx.Repo.PullRequest.HeadInfoSubURL = url.PathEscape(ctx.Doer.Name) + ":" + util.PathEscapeSegments(ctx.Repo.BranchName)
	}
}

func issues(ctx *context.Context, milestoneID, projectID int64, isPullOption util.OptionalBool) {
	var err error
	viewType := ctx.FormString("type")
	sortType := ctx.FormString("sort")
	types := []string{"all", "your_repositories", "assigned", "created_by", "mentioned", "review_requested"}
	if !util.IsStringInSlice(viewType, types, true) {
		viewType = "all"
	}

	var (
		assigneeID        = ctx.FormInt64("assignee")
		posterID          int64
		mentionedID       int64
		reviewRequestedID int64
		forceEmpty        bool
	)

	if ctx.IsSigned {
		switch viewType {
		case "created_by":
			posterID = ctx.Doer.ID
		case "mentioned":
			mentionedID = ctx.Doer.ID
		case "assigned":
			assigneeID = ctx.Doer.ID
		case "review_requested":
			reviewRequestedID = ctx.Doer.ID
		}
	}

	repo := ctx.Repo.Repository
	var labelIDs []int64
	selectLabels := ctx.FormString("labels")
	if len(selectLabels) > 0 && selectLabels != "0" {
		labelIDs, err = base.StringsToInt64s(strings.Split(selectLabels, ","))
		if err != nil {
			ctx.ServerError("StringsToInt64s", err)
			return
		}
	}

	keyword := strings.Trim(ctx.FormString("q"), " ")
	if bytes.Contains([]byte(keyword), []byte{0x00}) {
		keyword = ""
	}

	var issueIDs []int64
	if len(keyword) > 0 {
		issueIDs, err = issue_indexer.SearchIssuesByKeyword(ctx, []int64{repo.ID}, keyword)
		if err != nil {
			if issue_indexer.IsAvailable() {
				ctx.ServerError("issueIndexer.Search", err)
				return
			}
			ctx.Data["IssueIndexerUnavailable"] = true
		}
		if len(issueIDs) == 0 {
			forceEmpty = true
		}
	}

	var issueStats *models.IssueStats
	if forceEmpty {
		issueStats = &models.IssueStats{}
	} else {
		issueStats, err = models.GetIssueStats(&models.IssueStatsOptions{
			RepoID:            repo.ID,
			Labels:            selectLabels,
			MilestoneID:       milestoneID,
			AssigneeID:        assigneeID,
			MentionedID:       mentionedID,
			PosterID:          posterID,
			ReviewRequestedID: reviewRequestedID,
			IsPull:            isPullOption,
			IssueIDs:          issueIDs,
		})
		if err != nil {
			ctx.ServerError("GetIssueStats", err)
			return
		}
	}

	isShowClosed := ctx.FormString("state") == "closed"
	// if open issues are zero and close don't, use closed as default
	if len(ctx.FormString("state")) == 0 && issueStats.OpenCount == 0 && issueStats.ClosedCount != 0 {
		isShowClosed = true
	}

	page := ctx.FormInt("page")
	if page <= 1 {
		page = 1
	}

	var total int
	if !isShowClosed {
		total = int(issueStats.OpenCount)
	} else {
		total = int(issueStats.ClosedCount)
	}
	pager := context.NewPagination(total, setting.UI.IssuePagingNum, page, 5)

	var mileIDs []int64
	if milestoneID > 0 {
		mileIDs = []int64{milestoneID}
	}

	var issues []*models.Issue
	if forceEmpty {
		issues = []*models.Issue{}
	} else {
		issues, err = models.Issues(&models.IssuesOptions{
			ListOptions: db.ListOptions{
				Page:     pager.Paginater.Current(),
				PageSize: setting.UI.IssuePagingNum,
			},
			RepoID:            repo.ID,
			AssigneeID:        assigneeID,
			PosterID:          posterID,
			MentionedID:       mentionedID,
			ReviewRequestedID: reviewRequestedID,
			MilestoneIDs:      mileIDs,
			ProjectID:         projectID,
			IsClosed:          util.OptionalBoolOf(isShowClosed),
			IsPull:            isPullOption,
			LabelIDs:          labelIDs,
			SortType:          sortType,
			IssueIDs:          issueIDs,
		})
		if err != nil {
			ctx.ServerError("Issues", err)
			return
		}
	}

	issueList := models.IssueList(issues)
	approvalCounts, err := issueList.GetApprovalCounts()
	if err != nil {
		ctx.ServerError("ApprovalCounts", err)
		return
	}

	// Get posters.
	for i := range issues {
		// Check read status
		if !ctx.IsSigned {
			issues[i].IsRead = true
		} else if err = issues[i].GetIsRead(ctx.Doer.ID); err != nil {
			ctx.ServerError("GetIsRead", err)
			return
		}
	}

	commitStatuses, lastStatus, err := pull_service.GetIssuesAllCommitStatus(ctx, issues)
	if err != nil {
		ctx.ServerError("GetIssuesAllCommitStatus", err)
		return
	}

	ctx.Data["Issues"] = issues
	ctx.Data["CommitLastStatus"] = lastStatus
	ctx.Data["CommitStatuses"] = commitStatuses

	// Get assignees.
	ctx.Data["Assignees"], err = models.GetRepoAssignees(repo)
	if err != nil {
		ctx.ServerError("GetAssignees", err)
		return
	}

	handleTeamMentions(ctx)
	if ctx.Written() {
		return
	}

	labels, err := models.GetLabelsByRepoID(repo.ID, "", db.ListOptions{})
	if err != nil {
		ctx.ServerError("GetLabelsByRepoID", err)
		return
	}

	if repo.Owner.IsOrganization() {
		orgLabels, err := models.GetLabelsByOrgID(repo.Owner.ID, ctx.FormString("sort"), db.ListOptions{})
		if err != nil {
			ctx.ServerError("GetLabelsByOrgID", err)
			return
		}

		ctx.Data["OrgLabels"] = orgLabels
		labels = append(labels, orgLabels...)
	}

	for _, l := range labels {
		l.LoadSelectedLabelsAfterClick(labelIDs)
	}
	ctx.Data["Labels"] = labels
	ctx.Data["NumLabels"] = len(labels)

	if ctx.FormInt64("assignee") == 0 {
		assigneeID = 0 // Reset ID to prevent unexpected selection of assignee.
	}

	ctx.Data["IssueRefEndNames"], ctx.Data["IssueRefURLs"] = issue_service.GetRefEndNamesAndURLs(issues, ctx.Repo.RepoLink)

	ctx.Data["ApprovalCounts"] = func(issueID int64, typ string) int64 {
		counts, ok := approvalCounts[issueID]
		if !ok || len(counts) == 0 {
			return 0
		}
		reviewTyp := models.ReviewTypeApprove
		if typ == "reject" {
			reviewTyp = models.ReviewTypeReject
		} else if typ == "waiting" {
			reviewTyp = models.ReviewTypeRequest
		}
		for _, count := range counts {
			if count.Type == reviewTyp {
				return count.Count
			}
		}
		return 0
	}

	if ctx.Repo.CanWriteIssuesOrPulls(ctx.Params(":type") == "pulls") {
		projects, _, err := project_model.GetProjects(project_model.SearchOptions{
			RepoID:   repo.ID,
			Type:     project_model.TypeRepository,
			IsClosed: util.OptionalBoolOf(isShowClosed),
		})
		if err != nil {
			ctx.ServerError("GetProjects", err)
			return
		}
		ctx.Data["Projects"] = projects
	}

	ctx.Data["IssueStats"] = issueStats
	ctx.Data["SelLabelIDs"] = labelIDs
	ctx.Data["SelectLabels"] = selectLabels
	ctx.Data["ViewType"] = viewType
	ctx.Data["SortType"] = sortType
	ctx.Data["MilestoneID"] = milestoneID
	ctx.Data["AssigneeID"] = assigneeID
	ctx.Data["IsShowClosed"] = isShowClosed
	ctx.Data["Keyword"] = keyword
	if isShowClosed {
		ctx.Data["State"] = "closed"
	} else {
		ctx.Data["State"] = "open"
	}

	pager.AddParam(ctx, "q", "Keyword")
	pager.AddParam(ctx, "type", "ViewType")
	pager.AddParam(ctx, "sort", "SortType")
	pager.AddParam(ctx, "state", "State")
	pager.AddParam(ctx, "labels", "SelectLabels")
	pager.AddParam(ctx, "milestone", "MilestoneID")
	pager.AddParam(ctx, "assignee", "AssigneeID")
	ctx.Data["Page"] = pager
}

// Issues render issues page
func Issues(ctx *context.Context) {
	isPullList := ctx.Params(":type") == "pulls"
	if isPullList {
		MustAllowPulls(ctx)
		if ctx.Written() {
			return
		}
		ctx.Data["Title"] = ctx.Tr("repo.pulls")
		ctx.Data["PageIsPullList"] = true
	} else {
		MustEnableIssues(ctx)
		if ctx.Written() {
			return
		}
		ctx.Data["Title"] = ctx.Tr("repo.issues")
		ctx.Data["PageIsIssueList"] = true
		ctx.Data["NewIssueChooseTemplate"] = len(ctx.IssueTemplatesFromDefaultBranch()) > 0
	}

	issues(ctx, ctx.FormInt64("milestone"), ctx.FormInt64("project"), util.OptionalBoolOf(isPullList))
	if ctx.Written() {
		return
	}

	var err error
	// Get milestones
	ctx.Data["Milestones"], _, err = issues_model.GetMilestones(issues_model.GetMilestonesOption{
		RepoID: ctx.Repo.Repository.ID,
		State:  api.StateType(ctx.FormString("state")),
	})
	if err != nil {
		ctx.ServerError("GetAllRepoMilestones", err)
		return
	}

	ctx.Data["CanWriteIssuesOrPulls"] = ctx.Repo.CanWriteIssuesOrPulls(isPullList)

	ctx.HTML(http.StatusOK, tplIssues)
}

// RetrieveRepoMilestonesAndAssignees find all the milestones and assignees of a repository
func RetrieveRepoMilestonesAndAssignees(ctx *context.Context, repo *repo_model.Repository) {
	var err error
	ctx.Data["OpenMilestones"], _, err = issues_model.GetMilestones(issues_model.GetMilestonesOption{
		RepoID: repo.ID,
		State:  api.StateOpen,
	})
	if err != nil {
		ctx.ServerError("GetMilestones", err)
		return
	}
	ctx.Data["ClosedMilestones"], _, err = issues_model.GetMilestones(issues_model.GetMilestonesOption{
		RepoID: repo.ID,
		State:  api.StateClosed,
	})
	if err != nil {
		ctx.ServerError("GetMilestones", err)
		return
	}

	ctx.Data["Assignees"], err = models.GetRepoAssignees(repo)
	if err != nil {
		ctx.ServerError("GetAssignees", err)
		return
	}

	handleTeamMentions(ctx)
}

func retrieveProjects(ctx *context.Context, repo *repo_model.Repository) {
	var err error

	ctx.Data["OpenProjects"], _, err = project_model.GetProjects(project_model.SearchOptions{
		RepoID:   repo.ID,
		Page:     -1,
		IsClosed: util.OptionalBoolFalse,
		Type:     project_model.TypeRepository,
	})
	if err != nil {
		ctx.ServerError("GetProjects", err)
		return
	}

	ctx.Data["ClosedProjects"], _, err = project_model.GetProjects(project_model.SearchOptions{
		RepoID:   repo.ID,
		Page:     -1,
		IsClosed: util.OptionalBoolTrue,
		Type:     project_model.TypeRepository,
	})
	if err != nil {
		ctx.ServerError("GetProjects", err)
		return
	}
}

// repoReviewerSelection items to bee shown
type repoReviewerSelection struct {
	IsTeam    bool
	Team      *organization.Team
	User      *user_model.User
	Review    *models.Review
	CanChange bool
	Checked   bool
	ItemID    int64
}

// RetrieveRepoReviewers find all reviewers of a repository
func RetrieveRepoReviewers(ctx *context.Context, repo *repo_model.Repository, issue *models.Issue, canChooseReviewer bool) {
	ctx.Data["CanChooseReviewer"] = canChooseReviewer

	originalAuthorReviews, err := models.GetReviewersFromOriginalAuthorsByIssueID(issue.ID)
	if err != nil {
		ctx.ServerError("GetReviewersFromOriginalAuthorsByIssueID", err)
		return
	}
	ctx.Data["OriginalReviews"] = originalAuthorReviews

	reviews, err := models.GetReviewersByIssueID(issue.ID)
	if err != nil {
		ctx.ServerError("GetReviewersByIssueID", err)
		return
	}

	if len(reviews) == 0 && !canChooseReviewer {
		return
	}

	var (
		pullReviews         []*repoReviewerSelection
		reviewersResult     []*repoReviewerSelection
		teamReviewersResult []*repoReviewerSelection
		teamReviewers       []*organization.Team
		reviewers           []*user_model.User
	)

	if canChooseReviewer {
		posterID := issue.PosterID
		if issue.OriginalAuthorID > 0 {
			posterID = 0
		}

		reviewers, err = models.GetReviewers(repo, ctx.Doer.ID, posterID)
		if err != nil {
			ctx.ServerError("GetReviewers", err)
			return
		}

		teamReviewers, err = models.GetReviewerTeams(repo)
		if err != nil {
			ctx.ServerError("GetReviewerTeams", err)
			return
		}

		if len(reviewers) > 0 {
			reviewersResult = make([]*repoReviewerSelection, 0, len(reviewers))
		}

		if len(teamReviewers) > 0 {
			teamReviewersResult = make([]*repoReviewerSelection, 0, len(teamReviewers))
		}
	}

	pullReviews = make([]*repoReviewerSelection, 0, len(reviews))

	for _, review := range reviews {
		tmp := &repoReviewerSelection{
			Checked: review.Type == models.ReviewTypeRequest,
			Review:  review,
			ItemID:  review.ReviewerID,
		}
		if review.ReviewerTeamID > 0 {
			tmp.IsTeam = true
			tmp.ItemID = -review.ReviewerTeamID
		}

		if ctx.Repo.IsAdmin() {
			// Admin can dismiss or re-request any review requests
			tmp.CanChange = true
		} else if ctx.Doer != nil && ctx.Doer.ID == review.ReviewerID && review.Type == models.ReviewTypeRequest {
			// A user can refuse review requests
			tmp.CanChange = true
		} else if (canChooseReviewer || (ctx.Doer != nil && ctx.Doer.ID == issue.PosterID)) && review.Type != models.ReviewTypeRequest &&
			ctx.Doer.ID != review.ReviewerID {
			// The poster of the PR, a manager, or official reviewers can re-request review from other reviewers
			tmp.CanChange = true
		}

		pullReviews = append(pullReviews, tmp)

		if canChooseReviewer {
			if tmp.IsTeam {
				teamReviewersResult = append(teamReviewersResult, tmp)
			} else {
				reviewersResult = append(reviewersResult, tmp)
			}
		}
	}

	if len(pullReviews) > 0 {
		// Drop all non-existing users and teams from the reviews
		currentPullReviewers := make([]*repoReviewerSelection, 0, len(pullReviews))
		for _, item := range pullReviews {
			if item.Review.ReviewerID > 0 {
				if err = item.Review.LoadReviewer(); err != nil {
					if user_model.IsErrUserNotExist(err) {
						continue
					}
					ctx.ServerError("LoadReviewer", err)
					return
				}
				item.User = item.Review.Reviewer
			} else if item.Review.ReviewerTeamID > 0 {
				if err = item.Review.LoadReviewerTeam(); err != nil {
					if organization.IsErrTeamNotExist(err) {
						continue
					}
					ctx.ServerError("LoadReviewerTeam", err)
					return
				}
				item.Team = item.Review.ReviewerTeam
			} else {
				continue
			}

			currentPullReviewers = append(currentPullReviewers, item)
		}
		ctx.Data["PullReviewers"] = currentPullReviewers
	}

	if canChooseReviewer && reviewersResult != nil {
		preadded := len(reviewersResult)
		for _, reviewer := range reviewers {
			found := false
		reviewAddLoop:
			for _, tmp := range reviewersResult[:preadded] {
				if tmp.ItemID == reviewer.ID {
					tmp.User = reviewer
					found = true
					break reviewAddLoop
				}
			}

			if found {
				continue
			}

			reviewersResult = append(reviewersResult, &repoReviewerSelection{
				IsTeam:    false,
				CanChange: true,
				User:      reviewer,
				ItemID:    reviewer.ID,
			})
		}

		ctx.Data["Reviewers"] = reviewersResult
	}

	if canChooseReviewer && teamReviewersResult != nil {
		preadded := len(teamReviewersResult)
		for _, team := range teamReviewers {
			found := false
		teamReviewAddLoop:
			for _, tmp := range teamReviewersResult[:preadded] {
				if tmp.ItemID == -team.ID {
					tmp.Team = team
					found = true
					break teamReviewAddLoop
				}
			}

			if found {
				continue
			}

			teamReviewersResult = append(teamReviewersResult, &repoReviewerSelection{
				IsTeam:    true,
				CanChange: true,
				Team:      team,
				ItemID:    -team.ID,
			})
		}

		ctx.Data["TeamReviewers"] = teamReviewersResult
	}
}

// RetrieveRepoMetas find all the meta information of a repository
func RetrieveRepoMetas(ctx *context.Context, repo *repo_model.Repository, isPull bool) []*models.Label {
	if !ctx.Repo.CanWriteIssuesOrPulls(isPull) {
		return nil
	}

	labels, err := models.GetLabelsByRepoID(repo.ID, "", db.ListOptions{})
	if err != nil {
		ctx.ServerError("GetLabelsByRepoID", err)
		return nil
	}
	ctx.Data["Labels"] = labels
	if repo.Owner.IsOrganization() {
		orgLabels, err := models.GetLabelsByOrgID(repo.Owner.ID, ctx.FormString("sort"), db.ListOptions{})
		if err != nil {
			return nil
		}

		ctx.Data["OrgLabels"] = orgLabels
		labels = append(labels, orgLabels...)
	}

	RetrieveRepoMilestonesAndAssignees(ctx, repo)
	if ctx.Written() {
		return nil
	}

	retrieveProjects(ctx, repo)
	if ctx.Written() {
		return nil
	}

	brs, _, err := ctx.Repo.GitRepo.GetBranchNames(0, 0)
	if err != nil {
		ctx.ServerError("GetBranches", err)
		return nil
	}
	ctx.Data["Branches"] = brs

	// Contains true if the user can create issue dependencies
	ctx.Data["CanCreateIssueDependencies"] = ctx.Repo.CanCreateIssueDependencies(ctx.Doer, isPull)

	return labels
}

func getFileContentFromDefaultBranch(ctx *context.Context, filename string) (string, bool) {
	var bytes []byte

	if ctx.Repo.Commit == nil {
		var err error
		ctx.Repo.Commit, err = ctx.Repo.GitRepo.GetBranchCommit(ctx.Repo.Repository.DefaultBranch)
		if err != nil {
			return "", false
		}
	}

	entry, err := ctx.Repo.Commit.GetTreeEntryByPath(filename)
	if err != nil {
		return "", false
	}
	if entry.Blob().Size() >= setting.UI.MaxDisplayFileSize {
		return "", false
	}
	r, err := entry.Blob().DataAsync()
	if err != nil {
		return "", false
	}
	defer r.Close()
	bytes, err = io.ReadAll(r)
	if err != nil {
		return "", false
	}
	return string(bytes), true
}

func setTemplateIfExists(ctx *context.Context, ctxDataKey string, possibleDirs, possibleFiles []string) {
	templateCandidates := make([]string, 0, len(possibleFiles))
	if ctx.FormString("template") != "" {
		for _, dirName := range possibleDirs {
			templateCandidates = append(templateCandidates, path.Join(dirName, ctx.FormString("template")))
		}
	}
	templateCandidates = append(templateCandidates, possibleFiles...) // Append files to the end because they should be fallback
	for _, filename := range templateCandidates {
		templateContent, found := getFileContentFromDefaultBranch(ctx, filename)
		if found {
			var meta api.IssueTemplate
			templateBody, err := markdown.ExtractMetadata(templateContent, &meta)
			if err != nil {
				log.Debug("could not extract metadata from %s [%s]: %v", filename, ctx.Repo.Repository.FullName(), err)
				ctx.Data[ctxDataKey] = templateContent
				return
			}
			ctx.Data[issueTemplateTitleKey] = meta.Title
			ctx.Data[ctxDataKey] = templateBody
			labelIDs := make([]string, 0, len(meta.Labels))
			if repoLabels, err := models.GetLabelsByRepoID(ctx.Repo.Repository.ID, "", db.ListOptions{}); err == nil {
				ctx.Data["Labels"] = repoLabels
				if ctx.Repo.Owner.IsOrganization() {
					if orgLabels, err := models.GetLabelsByOrgID(ctx.Repo.Owner.ID, ctx.FormString("sort"), db.ListOptions{}); err == nil {
						ctx.Data["OrgLabels"] = orgLabels
						repoLabels = append(repoLabels, orgLabels...)
					}
				}

				for _, metaLabel := range meta.Labels {
					for _, repoLabel := range repoLabels {
						if strings.EqualFold(repoLabel.Name, metaLabel) {
							repoLabel.IsChecked = true
							labelIDs = append(labelIDs, strconv.FormatInt(repoLabel.ID, 10))
							break
						}
					}
				}
			}
			ctx.Data["HasSelectedLabel"] = len(labelIDs) > 0
			ctx.Data["label_ids"] = strings.Join(labelIDs, ",")
			ctx.Data["Reference"] = meta.Ref
			ctx.Data["RefEndName"] = git.RefEndName(meta.Ref)
			return
		}
	}
}

// NewIssue render creating issue page
func NewIssue(ctx *context.Context) {
	ctx.Data["Title"] = ctx.Tr("repo.issues.new")
	ctx.Data["PageIsIssueList"] = true
	ctx.Data["NewIssueChooseTemplate"] = len(ctx.IssueTemplatesFromDefaultBranch()) > 0
	ctx.Data["RequireTribute"] = true
	ctx.Data["PullRequestWorkInProgressPrefixes"] = setting.Repository.PullRequest.WorkInProgressPrefixes
	title := ctx.FormString("title")
	ctx.Data["TitleQuery"] = title
	body := ctx.FormString("body")
	ctx.Data["BodyQuery"] = body

	ctx.Data["IsProjectsEnabled"] = ctx.Repo.CanRead(unit.TypeProjects)
	ctx.Data["IsAttachmentEnabled"] = setting.Attachment.Enabled
	upload.AddUploadContext(ctx, "comment")

	milestoneID := ctx.FormInt64("milestone")
	if milestoneID > 0 {
		milestone, err := issues_model.GetMilestoneByRepoID(ctx, ctx.Repo.Repository.ID, milestoneID)
		if err != nil {
			log.Error("GetMilestoneByID: %d: %v", milestoneID, err)
		} else {
			ctx.Data["milestone_id"] = milestoneID
			ctx.Data["Milestone"] = milestone
		}
	}

	projectID := ctx.FormInt64("project")
	if projectID > 0 {
		project, err := project_model.GetProjectByID(projectID)
		if err != nil {
			log.Error("GetProjectByID: %d: %v", projectID, err)
		} else if project.RepoID != ctx.Repo.Repository.ID {
			log.Error("GetProjectByID: %d: %v", projectID, fmt.Errorf("project[%d] not in repo [%d]", project.ID, ctx.Repo.Repository.ID))
		} else {
			ctx.Data["project_id"] = projectID
			ctx.Data["Project"] = project
		}

		if len(ctx.Req.URL.Query().Get("project")) > 0 {
			ctx.Data["redirect_after_creation"] = "project"
		}
	}

	RetrieveRepoMetas(ctx, ctx.Repo.Repository, false)
	setTemplateIfExists(ctx, issueTemplateKey, context.IssueTemplateDirCandidates, IssueTemplateCandidates)
	if ctx.Written() {
		return
	}

	ctx.Data["HasIssuesOrPullsWritePermission"] = ctx.Repo.CanWrite(unit.TypeIssues)

	ctx.HTML(http.StatusOK, tplIssueNew)
}

// NewIssueChooseTemplate render creating issue from template page
func NewIssueChooseTemplate(ctx *context.Context) {
	ctx.Data["Title"] = ctx.Tr("repo.issues.new")
	ctx.Data["PageIsIssueList"] = true

	issueTemplates := ctx.IssueTemplatesFromDefaultBranch()
	ctx.Data["IssueTemplates"] = issueTemplates

	if len(issueTemplates) == 0 {
		// The "issues/new" and "issues/new/choose" share the same query parameters "project" and "milestone", if no template here, just redirect to the "issues/new" page with these parameters.
		ctx.Redirect(fmt.Sprintf("%s/issues/new?%s", ctx.Repo.Repository.HTMLURL(), ctx.Req.URL.RawQuery), http.StatusSeeOther)
		return
	}

	ctx.Data["milestone"] = ctx.FormInt64("milestone")
	ctx.Data["project"] = ctx.FormInt64("project")

	ctx.HTML(http.StatusOK, tplIssueChoose)
}

// DeleteIssue deletes an issue
func DeleteIssue(ctx *context.Context) {
	issue := GetActionIssue(ctx)
	if ctx.Written() {
		return
	}

	if err := issue_service.DeleteIssue(ctx.Doer, ctx.Repo.GitRepo, issue); err != nil {
		ctx.ServerError("DeleteIssueByID", err)
		return
	}

	ctx.Redirect(fmt.Sprintf("%s/issues", ctx.Repo.Repository.HTMLURL()), http.StatusSeeOther)
}

// ValidateRepoMetas check and returns repository's meta information
func ValidateRepoMetas(ctx *context.Context, form forms.CreateIssueForm, isPull bool) ([]int64, []int64, int64, int64) {
	var (
		repo = ctx.Repo.Repository
		err  error
	)

	labels := RetrieveRepoMetas(ctx, ctx.Repo.Repository, isPull)
	if ctx.Written() {
		return nil, nil, 0, 0
	}

	var labelIDs []int64
	hasSelected := false
	// Check labels.
	if len(form.LabelIDs) > 0 {
		labelIDs, err = base.StringsToInt64s(strings.Split(form.LabelIDs, ","))
		if err != nil {
			return nil, nil, 0, 0
		}
		labelIDMark := base.Int64sToMap(labelIDs)

		for i := range labels {
			if labelIDMark[labels[i].ID] {
				labels[i].IsChecked = true
				hasSelected = true
			}
		}
	}

	ctx.Data["Labels"] = labels
	ctx.Data["HasSelectedLabel"] = hasSelected
	ctx.Data["label_ids"] = form.LabelIDs

	// Check milestone.
	milestoneID := form.MilestoneID
	if milestoneID > 0 {
		milestone, err := issues_model.GetMilestoneByRepoID(ctx, ctx.Repo.Repository.ID, milestoneID)
		if err != nil {
			ctx.ServerError("GetMilestoneByID", err)
			return nil, nil, 0, 0
		}
		if milestone.RepoID != repo.ID {
			ctx.ServerError("GetMilestoneByID", err)
			return nil, nil, 0, 0
		}
		ctx.Data["Milestone"] = milestone
		ctx.Data["milestone_id"] = milestoneID
	}

	if form.ProjectID > 0 {
		p, err := project_model.GetProjectByID(form.ProjectID)
		if err != nil {
			ctx.ServerError("GetProjectByID", err)
			return nil, nil, 0, 0
		}
		if p.RepoID != ctx.Repo.Repository.ID {
			ctx.NotFound("", nil)
			return nil, nil, 0, 0
		}

		ctx.Data["Project"] = p
		ctx.Data["project_id"] = form.ProjectID
	}

	// Check assignees
	var assigneeIDs []int64
	if len(form.AssigneeIDs) > 0 {
		assigneeIDs, err = base.StringsToInt64s(strings.Split(form.AssigneeIDs, ","))
		if err != nil {
			return nil, nil, 0, 0
		}

		// Check if the passed assignees actually exists and is assignable
		for _, aID := range assigneeIDs {
			assignee, err := user_model.GetUserByID(aID)
			if err != nil {
				ctx.ServerError("GetUserByID", err)
				return nil, nil, 0, 0
			}

			valid, err := models.CanBeAssigned(assignee, repo, isPull)
			if err != nil {
				ctx.ServerError("CanBeAssigned", err)
				return nil, nil, 0, 0
			}

			if !valid {
				ctx.ServerError("canBeAssigned", models.ErrUserDoesNotHaveAccessToRepo{UserID: aID, RepoName: repo.Name})
				return nil, nil, 0, 0
			}
		}
	}

	// Keep the old assignee id thingy for compatibility reasons
	if form.AssigneeID > 0 {
		assigneeIDs = append(assigneeIDs, form.AssigneeID)
	}

	return labelIDs, assigneeIDs, milestoneID, form.ProjectID
}

// NewIssuePost response for creating new issue
func NewIssuePost(ctx *context.Context) {
	form := web.GetForm(ctx).(*forms.CreateIssueForm)
	ctx.Data["Title"] = ctx.Tr("repo.issues.new")
	ctx.Data["PageIsIssueList"] = true
	ctx.Data["NewIssueChooseTemplate"] = len(ctx.IssueTemplatesFromDefaultBranch()) > 0
	ctx.Data["PullRequestWorkInProgressPrefixes"] = setting.Repository.PullRequest.WorkInProgressPrefixes
	ctx.Data["IsAttachmentEnabled"] = setting.Attachment.Enabled
	upload.AddUploadContext(ctx, "comment")

	var (
		repo        = ctx.Repo.Repository
		attachments []string
	)

	labelIDs, assigneeIDs, milestoneID, projectID := ValidateRepoMetas(ctx, *form, false)
	if ctx.Written() {
		return
	}

	if setting.Attachment.Enabled {
		attachments = form.Files
	}

	if ctx.HasError() {
		ctx.HTML(http.StatusOK, tplIssueNew)
		return
	}

	if util.IsEmptyString(form.Title) {
		ctx.RenderWithErr(ctx.Tr("repo.issues.new.title_empty"), tplIssueNew, form)
		return
	}

	issue := &models.Issue{
		RepoID:      repo.ID,
		Repo:        repo,
		Title:       form.Title,
		PosterID:    ctx.Doer.ID,
		Poster:      ctx.Doer,
		MilestoneID: milestoneID,
		Content:     form.Content,
		Ref:         form.Ref,
	}

	if err := issue_service.NewIssue(repo, issue, labelIDs, attachments, assigneeIDs); err != nil {
		if models.IsErrUserDoesNotHaveAccessToRepo(err) {
			ctx.Error(http.StatusBadRequest, "UserDoesNotHaveAccessToRepo", err.Error())
			return
		}
		ctx.ServerError("NewIssue", err)
		return
	}

	if projectID > 0 {
		if err := models.ChangeProjectAssign(issue, ctx.Doer, projectID); err != nil {
			ctx.ServerError("ChangeProjectAssign", err)
			return
		}
	}

	log.Trace("Issue created: %d/%d", repo.ID, issue.ID)
	if ctx.FormString("redirect_after_creation") == "project" {
		ctx.Redirect(ctx.Repo.RepoLink + "/projects/" + strconv.FormatInt(form.ProjectID, 10))
	} else {
		ctx.Redirect(issue.Link())
	}
}

// roleDescriptor returns the Role Descriptor for a comment in/with the given repo, poster and issue
func roleDescriptor(ctx stdCtx.Context, repo *repo_model.Repository, poster *user_model.User, issue *models.Issue) (models.RoleDescriptor, error) {
	perm, err := models.GetUserRepoPermission(ctx, repo, poster)
	if err != nil {
		return models.RoleDescriptorNone, err
	}

	// By default the poster has no roles on the comment.
	roleDescriptor := models.RoleDescriptorNone

	// Check if the poster is owner of the repo.
	if perm.IsOwner() {
		// If the poster isn't a admin, enable the owner role.
		if !poster.IsAdmin {
			roleDescriptor = roleDescriptor.WithRole(models.RoleDescriptorOwner)
		} else {

			// Otherwise check if poster is the real repo admin.
			ok, err := models.IsUserRealRepoAdmin(repo, poster)
			if err != nil {
				return models.RoleDescriptorNone, err
			}
			if ok {
				roleDescriptor = roleDescriptor.WithRole(models.RoleDescriptorOwner)
			}
		}
	}

	// Is the poster can write issues or pulls to the repo, enable the Writer role.
	// Only enable this if the poster doesn't have the owner role already.
	if !roleDescriptor.HasRole("Owner") && perm.CanWriteIssuesOrPulls(issue.IsPull) {
		roleDescriptor = roleDescriptor.WithRole(models.RoleDescriptorWriter)
	}

	// If the poster is the actual poster of the issue, enable Poster role.
	if issue.IsPoster(poster.ID) {
		roleDescriptor = roleDescriptor.WithRole(models.RoleDescriptorPoster)
	}

	return roleDescriptor, nil
}

func getBranchData(ctx *context.Context, issue *models.Issue) {
	ctx.Data["BaseBranch"] = nil
	ctx.Data["HeadBranch"] = nil
	ctx.Data["HeadUserName"] = nil
	ctx.Data["BaseName"] = ctx.Repo.Repository.OwnerName
	if issue.IsPull {
		pull := issue.PullRequest
		ctx.Data["BaseBranch"] = pull.BaseBranch
		ctx.Data["HeadBranch"] = pull.HeadBranch
		ctx.Data["HeadUserName"] = pull.MustHeadUserName()
	}
}

// ViewIssue render issue view page
func ViewIssue(ctx *context.Context) {
	if ctx.Params(":type") == "issues" {
		// If issue was requested we check if repo has external tracker and redirect
		extIssueUnit, err := ctx.Repo.Repository.GetUnit(unit.TypeExternalTracker)
		if err == nil && extIssueUnit != nil {
			if extIssueUnit.ExternalTrackerConfig().ExternalTrackerStyle == markup.IssueNameStyleNumeric || extIssueUnit.ExternalTrackerConfig().ExternalTrackerStyle == "" {
				metas := ctx.Repo.Repository.ComposeMetas()
				metas["index"] = ctx.Params(":index")
				res, err := vars.Expand(extIssueUnit.ExternalTrackerConfig().ExternalTrackerFormat, metas)
				if err != nil {
					log.Error("unable to expand template vars for issue url. issue: %s, err: %v", metas["index"], err)
					ctx.ServerError("Expand", err)
					return
				}
				ctx.Redirect(res)
				return
			}
		} else if err != nil && !repo_model.IsErrUnitTypeNotExist(err) {
			ctx.ServerError("GetUnit", err)
			return
		}
	}

	issue, err := models.GetIssueByIndex(ctx.Repo.Repository.ID, ctx.ParamsInt64(":index"))
	if err != nil {
		if models.IsErrIssueNotExist(err) {
			ctx.NotFound("GetIssueByIndex", err)
		} else {
			ctx.ServerError("GetIssueByIndex", err)
		}
		return
	}
	if issue.Repo == nil {
		issue.Repo = ctx.Repo.Repository
	}

	// Make sure type and URL matches.
	if ctx.Params(":type") == "issues" && issue.IsPull {
		ctx.Redirect(issue.Link())
		return
	} else if ctx.Params(":type") == "pulls" && !issue.IsPull {
		ctx.Redirect(issue.Link())
		return
	}

	if issue.IsPull {
		MustAllowPulls(ctx)
		if ctx.Written() {
			return
		}
		ctx.Data["PageIsPullList"] = true
		ctx.Data["PageIsPullConversation"] = true
	} else {
		MustEnableIssues(ctx)
		if ctx.Written() {
			return
		}
		ctx.Data["PageIsIssueList"] = true
		ctx.Data["NewIssueChooseTemplate"] = len(ctx.IssueTemplatesFromDefaultBranch()) > 0
	}

	if issue.IsPull && !ctx.Repo.CanRead(unit.TypeIssues) {
		ctx.Data["IssueType"] = "pulls"
	} else if !issue.IsPull && !ctx.Repo.CanRead(unit.TypePullRequests) {
		ctx.Data["IssueType"] = "issues"
	} else {
		ctx.Data["IssueType"] = "all"
	}

	ctx.Data["RequireTribute"] = true
	ctx.Data["IsProjectsEnabled"] = ctx.Repo.CanRead(unit.TypeProjects)
	ctx.Data["IsAttachmentEnabled"] = setting.Attachment.Enabled
	upload.AddUploadContext(ctx, "comment")

	if err = issue.LoadAttributes(); err != nil {
		ctx.ServerError("LoadAttributes", err)
		return
	}

	if err = filterXRefComments(ctx, issue); err != nil {
		ctx.ServerError("filterXRefComments", err)
		return
	}

	ctx.Data["Title"] = fmt.Sprintf("#%d - %s", issue.Index, issue.Title)

	iw := new(models.IssueWatch)
	if ctx.Doer != nil {
		iw.UserID = ctx.Doer.ID
		iw.IssueID = issue.ID
		iw.IsWatching, err = models.CheckIssueWatch(ctx.Doer, issue)
		if err != nil {
			ctx.ServerError("CheckIssueWatch", err)
			return
		}
	}
	ctx.Data["IssueWatch"] = iw

	issue.RenderedContent, err = markdown.RenderString(&markup.RenderContext{
		URLPrefix: ctx.Repo.RepoLink,
		Metas:     ctx.Repo.Repository.ComposeMetas(),
		GitRepo:   ctx.Repo.GitRepo,
		Ctx:       ctx,
	}, issue.Content)
	if err != nil {
		ctx.ServerError("RenderString", err)
		return
	}

	repo := ctx.Repo.Repository

	// Get more information if it's a pull request.
	if issue.IsPull {
		if issue.PullRequest.HasMerged {
			ctx.Data["DisableStatusChange"] = issue.PullRequest.HasMerged
			PrepareMergedViewPullInfo(ctx, issue)
		} else {
			PrepareViewPullInfo(ctx, issue)
			ctx.Data["DisableStatusChange"] = ctx.Data["IsPullRequestBroken"] == true && issue.IsClosed
		}
		if ctx.Written() {
			return
		}
	}

	// Metas.
	// Check labels.
	labelIDMark := make(map[int64]bool)
	for i := range issue.Labels {
		labelIDMark[issue.Labels[i].ID] = true
	}
	labels, err := models.GetLabelsByRepoID(repo.ID, "", db.ListOptions{})
	if err != nil {
		ctx.ServerError("GetLabelsByRepoID", err)
		return
	}
	ctx.Data["Labels"] = labels

	if repo.Owner.IsOrganization() {
		orgLabels, err := models.GetLabelsByOrgID(repo.Owner.ID, ctx.FormString("sort"), db.ListOptions{})
		if err != nil {
			ctx.ServerError("GetLabelsByOrgID", err)
			return
		}
		ctx.Data["OrgLabels"] = orgLabels

		labels = append(labels, orgLabels...)
	}

	hasSelected := false
	for i := range labels {
		if labelIDMark[labels[i].ID] {
			labels[i].IsChecked = true
			hasSelected = true
		}
	}
	ctx.Data["HasSelectedLabel"] = hasSelected

	// Check milestone and assignee.
	if ctx.Repo.CanWriteIssuesOrPulls(issue.IsPull) {
		RetrieveRepoMilestonesAndAssignees(ctx, repo)
		retrieveProjects(ctx, repo)

		if ctx.Written() {
			return
		}
	}

	if issue.IsPull {
		canChooseReviewer := ctx.Repo.CanWrite(unit.TypePullRequests)
		if !canChooseReviewer && ctx.Doer != nil && ctx.IsSigned {
			canChooseReviewer, err = models.IsOfficialReviewer(issue, ctx.Doer)
			if err != nil {
				ctx.ServerError("IsOfficialReviewer", err)
				return
			}
		}

		RetrieveRepoReviewers(ctx, repo, issue, canChooseReviewer)
		if ctx.Written() {
			return
		}
	}

	if ctx.IsSigned {
		// Update issue-user.
		if err = issue.ReadBy(ctx, ctx.Doer.ID); err != nil {
			ctx.ServerError("ReadBy", err)
			return
		}
	}

	var (
		role         models.RoleDescriptor
		ok           bool
		marked       = make(map[int64]models.RoleDescriptor)
		comment      *models.Comment
		participants = make([]*user_model.User, 1, 10)
	)
	if ctx.Repo.Repository.IsTimetrackerEnabled() {
		if ctx.IsSigned {
			// Deal with the stopwatch
			ctx.Data["IsStopwatchRunning"] = models.StopwatchExists(ctx.Doer.ID, issue.ID)
			if !ctx.Data["IsStopwatchRunning"].(bool) {
				var exists bool
				var sw *models.Stopwatch
				if exists, sw, err = models.HasUserStopwatch(ctx.Doer.ID); err != nil {
					ctx.ServerError("HasUserStopwatch", err)
					return
				}
				ctx.Data["HasUserStopwatch"] = exists
				if exists {
					// Add warning if the user has already a stopwatch
					var otherIssue *models.Issue
					if otherIssue, err = models.GetIssueByID(sw.IssueID); err != nil {
						ctx.ServerError("GetIssueByID", err)
						return
					}
					if err = otherIssue.LoadRepo(ctx); err != nil {
						ctx.ServerError("LoadRepo", err)
						return
					}
					// Add link to the issue of the already running stopwatch
					ctx.Data["OtherStopwatchURL"] = otherIssue.HTMLURL()
				}
			}
			ctx.Data["CanUseTimetracker"] = ctx.Repo.CanUseTimetracker(issue, ctx.Doer)
		} else {
			ctx.Data["CanUseTimetracker"] = false
		}
		if ctx.Data["WorkingUsers"], err = models.TotalTimes(&models.FindTrackedTimesOptions{IssueID: issue.ID}); err != nil {
			ctx.ServerError("TotalTimes", err)
			return
		}
	}

	// Check if the user can use the dependencies
	ctx.Data["CanCreateIssueDependencies"] = ctx.Repo.CanCreateIssueDependencies(ctx.Doer, issue.IsPull)

	// check if dependencies can be created across repositories
	ctx.Data["AllowCrossRepositoryDependencies"] = setting.Service.AllowCrossRepositoryDependencies

	if issue.ShowRole, err = roleDescriptor(ctx, repo, issue.Poster, issue); err != nil {
		ctx.ServerError("roleDescriptor", err)
		return
	}
	marked[issue.PosterID] = issue.ShowRole

	// Render comments and and fetch participants.
	participants[0] = issue.Poster
	for _, comment = range issue.Comments {
		comment.Issue = issue

		if err := comment.LoadPoster(); err != nil {
			ctx.ServerError("LoadPoster", err)
			return
		}

		if comment.Type == models.CommentTypeComment || comment.Type == models.CommentTypeReview {
			if err := comment.LoadAttachments(); err != nil {
				ctx.ServerError("LoadAttachments", err)
				return
			}

			comment.RenderedContent, err = markdown.RenderString(&markup.RenderContext{
				URLPrefix: ctx.Repo.RepoLink,
				Metas:     ctx.Repo.Repository.ComposeMetas(),
				GitRepo:   ctx.Repo.GitRepo,
				Ctx:       ctx,
			}, comment.Content)
			if err != nil {
				ctx.ServerError("RenderString", err)
				return
			}
			// Check tag.
			role, ok = marked[comment.PosterID]
			if ok {
				comment.ShowRole = role
				continue
			}

			comment.ShowRole, err = roleDescriptor(ctx, repo, comment.Poster, issue)
			if err != nil {
				ctx.ServerError("roleDescriptor", err)
				return
			}
			marked[comment.PosterID] = comment.ShowRole
			participants = addParticipant(comment.Poster, participants)
		} else if comment.Type == models.CommentTypeLabel {
			if err = comment.LoadLabel(); err != nil {
				ctx.ServerError("LoadLabel", err)
				return
			}
		} else if comment.Type == models.CommentTypeMilestone {
			if err = comment.LoadMilestone(); err != nil {
				ctx.ServerError("LoadMilestone", err)
				return
			}
			ghostMilestone := &issues_model.Milestone{
				ID:   -1,
				Name: ctx.Tr("repo.issues.deleted_milestone"),
			}
			if comment.OldMilestoneID > 0 && comment.OldMilestone == nil {
				comment.OldMilestone = ghostMilestone
			}
			if comment.MilestoneID > 0 && comment.Milestone == nil {
				comment.Milestone = ghostMilestone
			}
		} else if comment.Type == models.CommentTypeProject {

			if err = comment.LoadProject(); err != nil {
				ctx.ServerError("LoadProject", err)
				return
			}

			ghostProject := &project_model.Project{
				ID:    -1,
				Title: ctx.Tr("repo.issues.deleted_project"),
			}

			if comment.OldProjectID > 0 && comment.OldProject == nil {
				comment.OldProject = ghostProject
			}

			if comment.ProjectID > 0 && comment.Project == nil {
				comment.Project = ghostProject
			}

		} else if comment.Type == models.CommentTypeAssignees || comment.Type == models.CommentTypeReviewRequest {
			if err = comment.LoadAssigneeUserAndTeam(); err != nil {
				ctx.ServerError("LoadAssigneeUserAndTeam", err)
				return
			}
		} else if comment.Type == models.CommentTypeRemoveDependency || comment.Type == models.CommentTypeAddDependency {
			if err = comment.LoadDepIssueDetails(); err != nil {
				if !models.IsErrIssueNotExist(err) {
					ctx.ServerError("LoadDepIssueDetails", err)
					return
				}
			}
		} else if comment.Type == models.CommentTypeCode || comment.Type == models.CommentTypeReview || comment.Type == models.CommentTypeDismissReview {
			comment.RenderedContent, err = markdown.RenderString(&markup.RenderContext{
				URLPrefix: ctx.Repo.RepoLink,
				Metas:     ctx.Repo.Repository.ComposeMetas(),
				GitRepo:   ctx.Repo.GitRepo,
				Ctx:       ctx,
			}, comment.Content)
			if err != nil {
				ctx.ServerError("RenderString", err)
				return
			}
			if err = comment.LoadReview(); err != nil && !models.IsErrReviewNotExist(err) {
				ctx.ServerError("LoadReview", err)
				return
			}
			participants = addParticipant(comment.Poster, participants)
			if comment.Review == nil {
				continue
			}
			if err = comment.Review.LoadAttributes(ctx); err != nil {
				if !user_model.IsErrUserNotExist(err) {
					ctx.ServerError("Review.LoadAttributes", err)
					return
				}
				comment.Review.Reviewer = user_model.NewGhostUser()
			}
			if err = comment.Review.LoadCodeComments(ctx); err != nil {
				ctx.ServerError("Review.LoadCodeComments", err)
				return
			}
			for _, codeComments := range comment.Review.CodeComments {
				for _, lineComments := range codeComments {
					for _, c := range lineComments {
						// Check tag.
						role, ok = marked[c.PosterID]
						if ok {
							c.ShowRole = role
							continue
						}

						c.ShowRole, err = roleDescriptor(ctx, repo, c.Poster, issue)
						if err != nil {
							ctx.ServerError("roleDescriptor", err)
							return
						}
						marked[c.PosterID] = c.ShowRole
						participants = addParticipant(c.Poster, participants)
					}
				}
			}
			if err = comment.LoadResolveDoer(); err != nil {
				ctx.ServerError("LoadResolveDoer", err)
				return
			}
		} else if comment.Type == models.CommentTypePullRequestPush {
			participants = addParticipant(comment.Poster, participants)
			if err = comment.LoadPushCommits(ctx); err != nil {
				ctx.ServerError("LoadPushCommits", err)
				return
			}
		} else if comment.Type == models.CommentTypeAddTimeManual ||
			comment.Type == models.CommentTypeStopTracking {
			// drop error since times could be pruned from DB..
			_ = comment.LoadTime()
		}
	}

	// Combine multiple label assignments into a single comment
	combineLabelComments(issue)

	getBranchData(ctx, issue)
	if issue.IsPull {
		pull := issue.PullRequest
		pull.Issue = issue
		canDelete := false
		ctx.Data["AllowMerge"] = false

		if ctx.IsSigned {
			if err := pull.LoadHeadRepoCtx(ctx); err != nil {
				log.Error("LoadHeadRepo: %v", err)
			} else if pull.HeadRepo != nil {
				perm, err := models.GetUserRepoPermission(ctx, pull.HeadRepo, ctx.Doer)
				if err != nil {
					ctx.ServerError("GetUserRepoPermission", err)
					return
				}
				if perm.CanWrite(unit.TypeCode) {
					// Check if branch is not protected
					if pull.HeadBranch != pull.HeadRepo.DefaultBranch {
						if protected, err := models.IsProtectedBranch(pull.HeadRepo.ID, pull.HeadBranch); err != nil {
							log.Error("IsProtectedBranch: %v", err)
						} else if !protected {
							canDelete = true
							ctx.Data["DeleteBranchLink"] = issue.Link() + "/cleanup"
						}
					}
					ctx.Data["CanWriteToHeadRepo"] = true
				}
			}

			if err := pull.LoadBaseRepoCtx(ctx); err != nil {
				log.Error("LoadBaseRepo: %v", err)
			}
			perm, err := models.GetUserRepoPermission(ctx, pull.BaseRepo, ctx.Doer)
			if err != nil {
				ctx.ServerError("GetUserRepoPermission", err)
				return
			}
			ctx.Data["AllowMerge"], err = pull_service.IsUserAllowedToMerge(ctx, pull, perm, ctx.Doer)
			if err != nil {
				ctx.ServerError("IsUserAllowedToMerge", err)
				return
			}

			if ctx.Data["CanMarkConversation"], err = models.CanMarkConversation(issue, ctx.Doer); err != nil {
				ctx.ServerError("CanMarkConversation", err)
				return
			}
		}

		prUnit, err := repo.GetUnit(unit.TypePullRequests)
		if err != nil {
			ctx.ServerError("GetUnit", err)
			return
		}
		prConfig := prUnit.PullRequestsConfig()

		// Check correct values and select default
		if ms, ok := ctx.Data["MergeStyle"].(repo_model.MergeStyle); !ok ||
			!prConfig.IsMergeStyleAllowed(ms) {
			defaultMergeStyle := prConfig.GetDefaultMergeStyle()
			if prConfig.IsMergeStyleAllowed(defaultMergeStyle) && !ok {
				ctx.Data["MergeStyle"] = defaultMergeStyle
			} else if prConfig.AllowMerge {
				ctx.Data["MergeStyle"] = repo_model.MergeStyleMerge
			} else if prConfig.AllowRebase {
				ctx.Data["MergeStyle"] = repo_model.MergeStyleRebase
			} else if prConfig.AllowRebaseMerge {
				ctx.Data["MergeStyle"] = repo_model.MergeStyleRebaseMerge
			} else if prConfig.AllowSquash {
				ctx.Data["MergeStyle"] = repo_model.MergeStyleSquash
			} else if prConfig.AllowManualMerge {
				ctx.Data["MergeStyle"] = repo_model.MergeStyleManuallyMerged
			} else {
				ctx.Data["MergeStyle"] = ""
			}
		}
		if err = pull.LoadProtectedBranch(); err != nil {
			ctx.ServerError("LoadProtectedBranch", err)
			return
		}
		ctx.Data["ShowMergeInstructions"] = true
		if pull.ProtectedBranch != nil {
			var showMergeInstructions bool
			if ctx.Doer != nil {
				showMergeInstructions = pull.ProtectedBranch.CanUserPush(ctx.Doer.ID)
			}
			ctx.Data["IsBlockedByApprovals"] = !pull.ProtectedBranch.HasEnoughApprovals(ctx, pull)
			ctx.Data["IsBlockedByRejection"] = pull.ProtectedBranch.MergeBlockedByRejectedReview(ctx, pull)
			ctx.Data["IsBlockedByOfficialReviewRequests"] = pull.ProtectedBranch.MergeBlockedByOfficialReviewRequests(ctx, pull)
			ctx.Data["IsBlockedByOutdatedBranch"] = pull.ProtectedBranch.MergeBlockedByOutdatedBranch(pull)
			ctx.Data["GrantedApprovals"] = pull.ProtectedBranch.GetGrantedApprovalsCount(ctx, pull)
			ctx.Data["RequireSigned"] = pull.ProtectedBranch.RequireSignedCommits
			ctx.Data["ChangedProtectedFiles"] = pull.ChangedProtectedFiles
			ctx.Data["IsBlockedByChangedProtectedFiles"] = len(pull.ChangedProtectedFiles) != 0
			ctx.Data["ChangedProtectedFilesNum"] = len(pull.ChangedProtectedFiles)
			ctx.Data["ShowMergeInstructions"] = showMergeInstructions
		}
		ctx.Data["WillSign"] = false
		if ctx.Doer != nil {
			sign, key, _, err := asymkey_service.SignMerge(ctx, pull, ctx.Doer, pull.BaseRepo.RepoPath(), pull.BaseBranch, pull.GetGitRefName())
			ctx.Data["WillSign"] = sign
			ctx.Data["SigningKey"] = key
			if err != nil {
				if asymkey_service.IsErrWontSign(err) {
					ctx.Data["WontSignReason"] = err.(*asymkey_service.ErrWontSign).Reason
				} else {
					ctx.Data["WontSignReason"] = "error"
					log.Error("Error whilst checking if could sign pr %d in repo %s. Error: %v", pull.ID, pull.BaseRepo.FullName(), err)
				}
			}
		} else {
			ctx.Data["WontSignReason"] = "not_signed_in"
		}

		isPullBranchDeletable := canDelete &&
			pull.HeadRepo != nil &&
			git.IsBranchExist(ctx, pull.HeadRepo.RepoPath(), pull.HeadBranch) &&
			(!pull.HasMerged || ctx.Data["HeadBranchCommitID"] == ctx.Data["PullHeadCommitID"])

		if isPullBranchDeletable && pull.HasMerged {
			exist, err := models.HasUnmergedPullRequestsByHeadInfo(ctx, pull.HeadRepoID, pull.HeadBranch)
			if err != nil {
				ctx.ServerError("HasUnmergedPullRequestsByHeadInfo", err)
				return
			}

			isPullBranchDeletable = !exist
		}
		ctx.Data["IsPullBranchDeletable"] = isPullBranchDeletable

		stillCanManualMerge := func() bool {
			if pull.HasMerged || issue.IsClosed || !ctx.IsSigned {
				return false
			}
			if pull.CanAutoMerge() || pull.IsWorkInProgress() || pull.IsChecking() {
				return false
			}
			if (ctx.Doer.IsAdmin || ctx.Repo.IsAdmin()) && prConfig.AllowManualMerge {
				return true
			}

			return false
		}

		ctx.Data["StillCanManualMerge"] = stillCanManualMerge()

		// Check if there is a pending pr merge
		ctx.Data["HasPendingPullRequestMerge"], ctx.Data["PendingPullRequestMerge"], err = pull_model.GetScheduledMergeByPullID(ctx, pull.ID)
		if err != nil {
			ctx.ServerError("GetScheduledMergeByPullID", err)
			return
		}
	}

	// Get Dependencies
	ctx.Data["BlockedByDependencies"], err = issue.BlockedByDependencies()
	if err != nil {
		ctx.ServerError("BlockedByDependencies", err)
		return
	}
	ctx.Data["BlockingDependencies"], err = issue.BlockingDependencies()
	if err != nil {
		ctx.ServerError("BlockingDependencies", err)
		return
	}

	ctx.Data["Participants"] = participants
	ctx.Data["NumParticipants"] = len(participants)
	ctx.Data["Issue"] = issue
	ctx.Data["Reference"] = issue.Ref
	ctx.Data["SignInLink"] = setting.AppSubURL + "/user/login?redirect_to=" + url.QueryEscape(ctx.Data["Link"].(string))
	ctx.Data["IsIssuePoster"] = ctx.IsSigned && issue.IsPoster(ctx.Doer.ID)
	ctx.Data["HasIssuesOrPullsWritePermission"] = ctx.Repo.CanWriteIssuesOrPulls(issue.IsPull)
	ctx.Data["HasProjectsWritePermission"] = ctx.Repo.CanWrite(unit.TypeProjects)
	ctx.Data["IsRepoAdmin"] = ctx.IsSigned && (ctx.Repo.IsAdmin() || ctx.Doer.IsAdmin)
	ctx.Data["LockReasons"] = setting.Repository.Issue.LockReasons
	ctx.Data["RefEndName"] = git.RefEndName(issue.Ref)

	var hiddenCommentTypes *big.Int
	if ctx.IsSigned {
		val, err := user_model.GetUserSetting(ctx.Doer.ID, user_model.SettingsKeyHiddenCommentTypes)
		if err != nil {
			ctx.ServerError("GetUserSetting", err)
			return
		}
		hiddenCommentTypes, _ = new(big.Int).SetString(val, 10) // we can safely ignore the failed conversion here
	}
	ctx.Data["ShouldShowCommentType"] = func(commentType models.CommentType) bool {
		return hiddenCommentTypes == nil || hiddenCommentTypes.Bit(int(commentType)) == 0
	}

	ctx.HTML(http.StatusOK, tplIssueView)
}

// GetActionIssue will return the issue which is used in the context.
func GetActionIssue(ctx *context.Context) *models.Issue {
	issue, err := models.GetIssueByIndex(ctx.Repo.Repository.ID, ctx.ParamsInt64(":index"))
	if err != nil {
		ctx.NotFoundOrServerError("GetIssueByIndex", models.IsErrIssueNotExist, err)
		return nil
	}
	issue.Repo = ctx.Repo.Repository
	checkIssueRights(ctx, issue)
	if ctx.Written() {
		return nil
	}
	if err = issue.LoadAttributes(); err != nil {
		ctx.ServerError("LoadAttributes", nil)
		return nil
	}
	return issue
}

func checkIssueRights(ctx *context.Context, issue *models.Issue) {
	if issue.IsPull && !ctx.Repo.CanRead(unit.TypePullRequests) ||
		!issue.IsPull && !ctx.Repo.CanRead(unit.TypeIssues) {
		ctx.NotFound("IssueOrPullRequestUnitNotAllowed", nil)
	}
}

func getActionIssues(ctx *context.Context) []*models.Issue {
	commaSeparatedIssueIDs := ctx.FormString("issue_ids")
	if len(commaSeparatedIssueIDs) == 0 {
		return nil
	}
	issueIDs := make([]int64, 0, 10)
	for _, stringIssueID := range strings.Split(commaSeparatedIssueIDs, ",") {
		issueID, err := strconv.ParseInt(stringIssueID, 10, 64)
		if err != nil {
			ctx.ServerError("ParseInt", err)
			return nil
		}
		issueIDs = append(issueIDs, issueID)
	}
	issues, err := models.GetIssuesByIDs(issueIDs)
	if err != nil {
		ctx.ServerError("GetIssuesByIDs", err)
		return nil
	}
	// Check access rights for all issues
	issueUnitEnabled := ctx.Repo.CanRead(unit.TypeIssues)
	prUnitEnabled := ctx.Repo.CanRead(unit.TypePullRequests)
	for _, issue := range issues {
		if issue.IsPull && !prUnitEnabled || !issue.IsPull && !issueUnitEnabled {
			ctx.NotFound("IssueOrPullRequestUnitNotAllowed", nil)
			return nil
		}
		if err = issue.LoadAttributes(); err != nil {
			ctx.ServerError("LoadAttributes", err)
			return nil
		}
	}
	return issues
}

// GetIssueInfo get an issue of a repository
func GetIssueInfo(ctx *context.Context) {
	issue, err := models.GetIssueWithAttrsByIndex(ctx.Repo.Repository.ID, ctx.ParamsInt64(":index"))
	if err != nil {
		if models.IsErrIssueNotExist(err) {
			ctx.Error(http.StatusNotFound)
		} else {
			ctx.Error(http.StatusInternalServerError, "GetIssueByIndex", err.Error())
		}
		return
	}
	ctx.JSON(http.StatusOK, convert.ToAPIIssue(issue))
}

// UpdateIssueTitle change issue's title
func UpdateIssueTitle(ctx *context.Context) {
	issue := GetActionIssue(ctx)
	if ctx.Written() {
		return
	}

	if !ctx.IsSigned || (!issue.IsPoster(ctx.Doer.ID) && !ctx.Repo.CanWriteIssuesOrPulls(issue.IsPull)) {
		ctx.Error(http.StatusForbidden)
		return
	}

	title := ctx.FormTrim("title")
	if len(title) == 0 {
		ctx.Error(http.StatusNoContent)
		return
	}

	if err := issue_service.ChangeTitle(issue, ctx.Doer, title); err != nil {
		ctx.ServerError("ChangeTitle", err)
		return
	}

	ctx.JSON(http.StatusOK, map[string]interface{}{
		"title": issue.Title,
	})
}

// UpdateIssueRef change issue's ref (branch)
func UpdateIssueRef(ctx *context.Context) {
	issue := GetActionIssue(ctx)
	if ctx.Written() {
		return
	}

	if !ctx.IsSigned || (!issue.IsPoster(ctx.Doer.ID) && !ctx.Repo.CanWriteIssuesOrPulls(issue.IsPull)) || issue.IsPull {
		ctx.Error(http.StatusForbidden)
		return
	}

	ref := ctx.FormTrim("ref")

	if err := issue_service.ChangeIssueRef(issue, ctx.Doer, ref); err != nil {
		ctx.ServerError("ChangeRef", err)
		return
	}

	ctx.JSON(http.StatusOK, map[string]interface{}{
		"ref": ref,
	})
}

// UpdateIssueContent change issue's content
func UpdateIssueContent(ctx *context.Context) {
	issue := GetActionIssue(ctx)
	if ctx.Written() {
		return
	}

	if !ctx.IsSigned || (ctx.Doer.ID != issue.PosterID && !ctx.Repo.CanWriteIssuesOrPulls(issue.IsPull)) {
		ctx.Error(http.StatusForbidden)
		return
	}

	if err := issue_service.ChangeContent(issue, ctx.Doer, ctx.Req.FormValue("content")); err != nil {
		ctx.ServerError("ChangeContent", err)
		return
	}

	// when update the request doesn't intend to update attachments (eg: change checkbox state), ignore attachment updates
	if !ctx.FormBool("ignore_attachments") {
		if err := updateAttachments(issue, ctx.FormStrings("files[]")); err != nil {
			ctx.ServerError("UpdateAttachments", err)
			return
		}
	}

	content, err := markdown.RenderString(&markup.RenderContext{
		URLPrefix: ctx.FormString("context"), // FIXME: <- IS THIS SAFE ?
		Metas:     ctx.Repo.Repository.ComposeMetas(),
		GitRepo:   ctx.Repo.GitRepo,
		Ctx:       ctx,
	}, issue.Content)
	if err != nil {
		ctx.ServerError("RenderString", err)
		return
	}

	ctx.JSON(http.StatusOK, map[string]interface{}{
		"content":     content,
		"attachments": attachmentsHTML(ctx, issue.Attachments, issue.Content),
	})
}

// UpdateIssueDeadline updates an issue deadline
func UpdateIssueDeadline(ctx *context.Context) {
	form := web.GetForm(ctx).(*api.EditDeadlineOption)
	issue, err := models.GetIssueByIndex(ctx.Repo.Repository.ID, ctx.ParamsInt64(":index"))
	if err != nil {
		if models.IsErrIssueNotExist(err) {
			ctx.NotFound("GetIssueByIndex", err)
		} else {
			ctx.Error(http.StatusInternalServerError, "GetIssueByIndex", err.Error())
		}
		return
	}

	if !ctx.Repo.CanWriteIssuesOrPulls(issue.IsPull) {
		ctx.Error(http.StatusForbidden, "", "Not repo writer")
		return
	}

	var deadlineUnix timeutil.TimeStamp
	var deadline time.Time
	if form.Deadline != nil && !form.Deadline.IsZero() {
		deadline = time.Date(form.Deadline.Year(), form.Deadline.Month(), form.Deadline.Day(),
			23, 59, 59, 0, time.Local)
		deadlineUnix = timeutil.TimeStamp(deadline.Unix())
	}

	if err := models.UpdateIssueDeadline(issue, deadlineUnix, ctx.Doer); err != nil {
		ctx.Error(http.StatusInternalServerError, "UpdateIssueDeadline", err.Error())
		return
	}

	ctx.JSON(http.StatusCreated, api.IssueDeadline{Deadline: &deadline})
}

// UpdateIssueMilestone change issue's milestone
func UpdateIssueMilestone(ctx *context.Context) {
	issues := getActionIssues(ctx)
	if ctx.Written() {
		return
	}

	milestoneID := ctx.FormInt64("id")
	for _, issue := range issues {
		oldMilestoneID := issue.MilestoneID
		if oldMilestoneID == milestoneID {
			continue
		}
		issue.MilestoneID = milestoneID
		if err := issue_service.ChangeMilestoneAssign(issue, ctx.Doer, oldMilestoneID); err != nil {
			ctx.ServerError("ChangeMilestoneAssign", err)
			return
		}
	}

	ctx.JSON(http.StatusOK, map[string]interface{}{
		"ok": true,
	})
}

// UpdateIssueAssignee change issue's or pull's assignee
func UpdateIssueAssignee(ctx *context.Context) {
	issues := getActionIssues(ctx)
	if ctx.Written() {
		return
	}

	assigneeID := ctx.FormInt64("id")
	action := ctx.FormString("action")

	for _, issue := range issues {
		switch action {
		case "clear":
			if err := issue_service.DeleteNotPassedAssignee(issue, ctx.Doer, []*user_model.User{}); err != nil {
				ctx.ServerError("ClearAssignees", err)
				return
			}
		default:
			assignee, err := user_model.GetUserByID(assigneeID)
			if err != nil {
				ctx.ServerError("GetUserByID", err)
				return
			}

			valid, err := models.CanBeAssigned(assignee, issue.Repo, issue.IsPull)
			if err != nil {
				ctx.ServerError("canBeAssigned", err)
				return
			}
			if !valid {
				ctx.ServerError("canBeAssigned", models.ErrUserDoesNotHaveAccessToRepo{UserID: assigneeID, RepoName: issue.Repo.Name})
				return
			}

			_, _, err = issue_service.ToggleAssignee(issue, ctx.Doer, assigneeID)
			if err != nil {
				ctx.ServerError("ToggleAssignee", err)
				return
			}
		}
	}
	ctx.JSON(http.StatusOK, map[string]interface{}{
		"ok": true,
	})
}

// UpdatePullReviewRequest add or remove review request
func UpdatePullReviewRequest(ctx *context.Context) {
	issues := getActionIssues(ctx)
	if ctx.Written() {
		return
	}

	reviewID := ctx.FormInt64("id")
	action := ctx.FormString("action")

	// TODO: Not support 'clear' now
	if action != "attach" && action != "detach" {
		ctx.Status(http.StatusForbidden)
		return
	}

	for _, issue := range issues {
		if err := issue.LoadRepo(ctx); err != nil {
			ctx.ServerError("issue.LoadRepo", err)
			return
		}

		if !issue.IsPull {
			log.Warn(
				"UpdatePullReviewRequest: refusing to add review request for non-PR issue %-v#%d",
				issue.Repo, issue.Index,
			)
			ctx.Status(http.StatusForbidden)
			return
		}
		if reviewID < 0 {
			// negative reviewIDs represent team requests
			if err := issue.Repo.GetOwner(ctx); err != nil {
				ctx.ServerError("issue.Repo.GetOwner", err)
				return
			}

			if !issue.Repo.Owner.IsOrganization() {
				log.Warn(
					"UpdatePullReviewRequest: refusing to add team review request for %s#%d owned by non organization UID[%d]",
					issue.Repo.FullName(), issue.Index, issue.Repo.ID,
				)
				ctx.Status(http.StatusForbidden)
				return
			}

			team, err := organization.GetTeamByID(-reviewID)
			if err != nil {
				ctx.ServerError("GetTeamByID", err)
				return
			}

			if team.OrgID != issue.Repo.OwnerID {
				log.Warn(
					"UpdatePullReviewRequest: refusing to add team review request for UID[%d] team %s to %s#%d owned by UID[%d]",
					team.OrgID, team.Name, issue.Repo.FullName(), issue.Index, issue.Repo.ID)
				ctx.Status(http.StatusForbidden)
				return
			}

			err = issue_service.IsValidTeamReviewRequest(ctx, team, ctx.Doer, action == "attach", issue)
			if err != nil {
				if models.IsErrNotValidReviewRequest(err) {
					log.Warn(
						"UpdatePullReviewRequest: refusing to add invalid team review request for UID[%d] team %s to %s#%d owned by UID[%d]: Error: %v",
						team.OrgID, team.Name, issue.Repo.FullName(), issue.Index, issue.Repo.ID,
						err,
					)
					ctx.Status(http.StatusForbidden)
					return
				}
				ctx.ServerError("IsValidTeamReviewRequest", err)
				return
			}

			_, err = issue_service.TeamReviewRequest(issue, ctx.Doer, team, action == "attach")
			if err != nil {
				ctx.ServerError("TeamReviewRequest", err)
				return
			}
			continue
		}

		reviewer, err := user_model.GetUserByID(reviewID)
		if err != nil {
			if user_model.IsErrUserNotExist(err) {
				log.Warn(
					"UpdatePullReviewRequest: requested reviewer [%d] for %-v to %-v#%d is not exist: Error: %v",
					reviewID, issue.Repo, issue.Index,
					err,
				)
				ctx.Status(http.StatusForbidden)
				return
			}
			ctx.ServerError("GetUserByID", err)
			return
		}

		err = issue_service.IsValidReviewRequest(ctx, reviewer, ctx.Doer, action == "attach", issue, nil)
		if err != nil {
			if models.IsErrNotValidReviewRequest(err) {
				log.Warn(
					"UpdatePullReviewRequest: refusing to add invalid review request for %-v to %-v#%d: Error: %v",
					reviewer, issue.Repo, issue.Index,
					err,
				)
				ctx.Status(http.StatusForbidden)
				return
			}
			ctx.ServerError("isValidReviewRequest", err)
			return
		}

		_, err = issue_service.ReviewRequest(issue, ctx.Doer, reviewer, action == "attach")
		if err != nil {
			ctx.ServerError("ReviewRequest", err)
			return
		}
	}

	ctx.JSON(http.StatusOK, map[string]interface{}{
		"ok": true,
	})
}

// SearchIssues searches for issues across the repositories that the user has access to
func SearchIssues(ctx *context.Context) {
	before, since, err := context.GetQueryBeforeSince(ctx)
	if err != nil {
		ctx.Error(http.StatusUnprocessableEntity, err.Error())
		return
	}

	var isClosed util.OptionalBool
	switch ctx.FormString("state") {
	case "closed":
		isClosed = util.OptionalBoolTrue
	case "all":
		isClosed = util.OptionalBoolNone
	default:
		isClosed = util.OptionalBoolFalse
	}

	// find repos user can access (for issue search)
	opts := &models.SearchRepoOptions{
		Private:     false,
		AllPublic:   true,
		TopicOnly:   false,
		Collaborate: util.OptionalBoolNone,
		// This needs to be a column that is not nil in fixtures or
		// MySQL will return different results when sorting by null in some cases
		OrderBy: db.SearchOrderByAlphabetically,
		Actor:   ctx.Doer,
	}
	if ctx.IsSigned {
		opts.Private = true
		opts.AllLimited = true
	}
	if ctx.FormString("owner") != "" {
		owner, err := user_model.GetUserByName(ctx.FormString("owner"))
		if err != nil {
			if user_model.IsErrUserNotExist(err) {
				ctx.Error(http.StatusBadRequest, "Owner not found", err.Error())
			} else {
				ctx.Error(http.StatusInternalServerError, "GetUserByName", err.Error())
			}
			return
		}
		opts.OwnerID = owner.ID
		opts.AllLimited = false
		opts.AllPublic = false
		opts.Collaborate = util.OptionalBoolFalse
	}
	if ctx.FormString("team") != "" {
		if ctx.FormString("owner") == "" {
			ctx.Error(http.StatusBadRequest, "", "Owner organisation is required for filtering on team")
			return
		}
		team, err := organization.GetTeam(opts.OwnerID, ctx.FormString("team"))
		if err != nil {
			if organization.IsErrTeamNotExist(err) {
				ctx.Error(http.StatusBadRequest, "Team not found", err.Error())
			} else {
				ctx.Error(http.StatusInternalServerError, "GetUserByName", err.Error())
			}
			return
		}
		opts.TeamID = team.ID
	}

	repoCond := models.SearchRepositoryCondition(opts)
	repoIDs, _, err := models.SearchRepositoryIDs(opts)
	if err != nil {
		ctx.Error(http.StatusInternalServerError, "SearchRepositoryByName", err.Error())
		return
	}

	var issues []*models.Issue
	var filteredCount int64

	keyword := ctx.FormTrim("q")
	if strings.IndexByte(keyword, 0) >= 0 {
		keyword = ""
	}
	var issueIDs []int64
	if len(keyword) > 0 && len(repoIDs) > 0 {
		if issueIDs, err = issue_indexer.SearchIssuesByKeyword(ctx, repoIDs, keyword); err != nil {
			ctx.Error(http.StatusInternalServerError, "SearchIssuesByKeyword", err.Error())
			return
		}
	}

	var isPull util.OptionalBool
	switch ctx.FormString("type") {
	case "pulls":
		isPull = util.OptionalBoolTrue
	case "issues":
		isPull = util.OptionalBoolFalse
	default:
		isPull = util.OptionalBoolNone
	}

	labels := ctx.FormTrim("labels")
	var includedLabelNames []string
	if len(labels) > 0 {
		includedLabelNames = strings.Split(labels, ",")
	}

	milestones := ctx.FormTrim("milestones")
	var includedMilestones []string
	if len(milestones) > 0 {
		includedMilestones = strings.Split(milestones, ",")
	}

	// this api is also used in UI,
	// so the default limit is set to fit UI needs
	limit := ctx.FormInt("limit")
	if limit == 0 {
		limit = setting.UI.IssuePagingNum
	} else if limit > setting.API.MaxResponseItems {
		limit = setting.API.MaxResponseItems
	}

	// Only fetch the issues if we either don't have a keyword or the search returned issues
	// This would otherwise return all issues if no issues were found by the search.
	if len(keyword) == 0 || len(issueIDs) > 0 || len(includedLabelNames) > 0 || len(includedMilestones) > 0 {
		issuesOpt := &models.IssuesOptions{
			ListOptions: db.ListOptions{
				Page:     ctx.FormInt("page"),
				PageSize: limit,
			},
			RepoCond:           repoCond,
			IsClosed:           isClosed,
			IssueIDs:           issueIDs,
			IncludedLabelNames: includedLabelNames,
			IncludeMilestones:  includedMilestones,
			SortType:           "priorityrepo",
			PriorityRepoID:     ctx.FormInt64("priority_repo_id"),
			IsPull:             isPull,
			UpdatedBeforeUnix:  before,
			UpdatedAfterUnix:   since,
		}

		ctxUserID := int64(0)
		if ctx.IsSigned {
			ctxUserID = ctx.Doer.ID
		}

		// Filter for: Created by User, Assigned to User, Mentioning User, Review of User Requested
		if ctx.FormBool("created") {
			issuesOpt.PosterID = ctxUserID
		}
		if ctx.FormBool("assigned") {
			issuesOpt.AssigneeID = ctxUserID
		}
		if ctx.FormBool("mentioned") {
			issuesOpt.MentionedID = ctxUserID
		}
		if ctx.FormBool("review_requested") {
			issuesOpt.ReviewRequestedID = ctxUserID
		}

		if issues, err = models.Issues(issuesOpt); err != nil {
			ctx.Error(http.StatusInternalServerError, "Issues", err.Error())
			return
		}

		issuesOpt.ListOptions = db.ListOptions{
			Page: -1,
		}
		if filteredCount, err = models.CountIssues(issuesOpt); err != nil {
			ctx.Error(http.StatusInternalServerError, "CountIssues", err.Error())
			return
		}
	}

	ctx.SetTotalCountHeader(filteredCount)
	ctx.JSON(http.StatusOK, convert.ToAPIIssueList(issues))
}

func getUserIDForFilter(ctx *context.Context, queryName string) int64 {
	userName := ctx.FormString(queryName)
	if len(userName) == 0 {
		return 0
	}

	user, err := user_model.GetUserByName(userName)
	if user_model.IsErrUserNotExist(err) {
		ctx.NotFound("", err)
		return 0
	}

	if err != nil {
		ctx.Error(http.StatusInternalServerError, err.Error())
		return 0
	}

	return user.ID
}

// ListIssues list the issues of a repository
func ListIssues(ctx *context.Context) {
	before, since, err := context.GetQueryBeforeSince(ctx)
	if err != nil {
		ctx.Error(http.StatusUnprocessableEntity, err.Error())
		return
	}

	var isClosed util.OptionalBool
	switch ctx.FormString("state") {
	case "closed":
		isClosed = util.OptionalBoolTrue
	case "all":
		isClosed = util.OptionalBoolNone
	default:
		isClosed = util.OptionalBoolFalse
	}

	var issues []*models.Issue
	var filteredCount int64

	keyword := ctx.FormTrim("q")
	if strings.IndexByte(keyword, 0) >= 0 {
		keyword = ""
	}
	var issueIDs []int64
	var labelIDs []int64
	if len(keyword) > 0 {
		issueIDs, err = issue_indexer.SearchIssuesByKeyword(ctx, []int64{ctx.Repo.Repository.ID}, keyword)
		if err != nil {
			ctx.Error(http.StatusInternalServerError, err.Error())
			return
		}
	}

	if splitted := strings.Split(ctx.FormString("labels"), ","); len(splitted) > 0 {
		labelIDs, err = models.GetLabelIDsInRepoByNames(ctx.Repo.Repository.ID, splitted)
		if err != nil {
			ctx.Error(http.StatusInternalServerError, err.Error())
			return
		}
	}

	var mileIDs []int64
	if part := strings.Split(ctx.FormString("milestones"), ","); len(part) > 0 {
		for i := range part {
			// uses names and fall back to ids
			// non existent milestones are discarded
			mile, err := issues_model.GetMilestoneByRepoIDANDName(ctx.Repo.Repository.ID, part[i])
			if err == nil {
				mileIDs = append(mileIDs, mile.ID)
				continue
			}
			if !issues_model.IsErrMilestoneNotExist(err) {
				ctx.Error(http.StatusInternalServerError, err.Error())
				return
			}
			id, err := strconv.ParseInt(part[i], 10, 64)
			if err != nil {
				continue
			}
			mile, err = issues_model.GetMilestoneByRepoID(ctx, ctx.Repo.Repository.ID, id)
			if err == nil {
				mileIDs = append(mileIDs, mile.ID)
				continue
			}
			if issues_model.IsErrMilestoneNotExist(err) {
				continue
			}
			ctx.Error(http.StatusInternalServerError, err.Error())
		}
	}

	listOptions := db.ListOptions{
		Page:     ctx.FormInt("page"),
		PageSize: convert.ToCorrectPageSize(ctx.FormInt("limit")),
	}

	var isPull util.OptionalBool
	switch ctx.FormString("type") {
	case "pulls":
		isPull = util.OptionalBoolTrue
	case "issues":
		isPull = util.OptionalBoolFalse
	default:
		isPull = util.OptionalBoolNone
	}

	// FIXME: we should be more efficient here
	createdByID := getUserIDForFilter(ctx, "created_by")
	if ctx.Written() {
		return
	}
	assignedByID := getUserIDForFilter(ctx, "assigned_by")
	if ctx.Written() {
		return
	}
	mentionedByID := getUserIDForFilter(ctx, "mentioned_by")
	if ctx.Written() {
		return
	}

	// Only fetch the issues if we either don't have a keyword or the search returned issues
	// This would otherwise return all issues if no issues were found by the search.
	if len(keyword) == 0 || len(issueIDs) > 0 || len(labelIDs) > 0 {
		issuesOpt := &models.IssuesOptions{
			ListOptions:       listOptions,
			RepoID:            ctx.Repo.Repository.ID,
			IsClosed:          isClosed,
			IssueIDs:          issueIDs,
			LabelIDs:          labelIDs,
			MilestoneIDs:      mileIDs,
			IsPull:            isPull,
			UpdatedBeforeUnix: before,
			UpdatedAfterUnix:  since,
			PosterID:          createdByID,
			AssigneeID:        assignedByID,
			MentionedID:       mentionedByID,
		}

		if issues, err = models.Issues(issuesOpt); err != nil {
			ctx.Error(http.StatusInternalServerError, err.Error())
			return
		}

		issuesOpt.ListOptions = db.ListOptions{
			Page: -1,
		}
		if filteredCount, err = models.CountIssues(issuesOpt); err != nil {
			ctx.Error(http.StatusInternalServerError, err.Error())
			return
		}
	}

	ctx.SetTotalCountHeader(filteredCount)
	ctx.JSON(http.StatusOK, convert.ToAPIIssueList(issues))
}

// UpdateIssueStatus change issue's status
func UpdateIssueStatus(ctx *context.Context) {
	issues := getActionIssues(ctx)
	if ctx.Written() {
		return
	}

	var isClosed bool
	switch action := ctx.FormString("action"); action {
	case "open":
		isClosed = false
	case "close":
		isClosed = true
	default:
		log.Warn("Unrecognized action: %s", action)
	}

	if _, err := models.IssueList(issues).LoadRepositories(); err != nil {
		ctx.ServerError("LoadRepositories", err)
		return
	}
	for _, issue := range issues {
		if issue.IsClosed != isClosed {
			if err := issue_service.ChangeStatus(issue, ctx.Doer, isClosed); err != nil {
				if models.IsErrDependenciesLeft(err) {
					ctx.JSON(http.StatusPreconditionFailed, map[string]interface{}{
						"error": "cannot close this issue because it still has open dependencies",
					})
					return
				}
				ctx.ServerError("ChangeStatus", err)
				return
			}
		}
	}
	ctx.JSON(http.StatusOK, map[string]interface{}{
		"ok": true,
	})
}

// NewComment create a comment for issue
func NewComment(ctx *context.Context) {
	form := web.GetForm(ctx).(*forms.CreateCommentForm)
	issue := GetActionIssue(ctx)
	if ctx.Written() {
		return
	}

	if !ctx.IsSigned || (ctx.Doer.ID != issue.PosterID && !ctx.Repo.CanReadIssuesOrPulls(issue.IsPull)) {
		if log.IsTrace() {
			if ctx.IsSigned {
				issueType := "issues"
				if issue.IsPull {
					issueType = "pulls"
				}
				log.Trace("Permission Denied: User %-v not the Poster (ID: %d) and cannot read %s in Repo %-v.\n"+
					"User in Repo has Permissions: %-+v",
					ctx.Doer,
					log.NewColoredIDValue(issue.PosterID),
					issueType,
					ctx.Repo.Repository,
					ctx.Repo.Permission)
			} else {
				log.Trace("Permission Denied: Not logged in")
			}
		}

		ctx.Error(http.StatusForbidden)
		return
	}

	if issue.IsLocked && !ctx.Repo.CanWriteIssuesOrPulls(issue.IsPull) && !ctx.Doer.IsAdmin {
		ctx.Flash.Error(ctx.Tr("repo.issues.comment_on_locked"))
		ctx.Redirect(issue.HTMLURL())
		return
	}

	var attachments []string
	if setting.Attachment.Enabled {
		attachments = form.Files
	}

	if ctx.HasError() {
		ctx.Flash.Error(ctx.Data["ErrorMsg"].(string))
		ctx.Redirect(issue.HTMLURL())
		return
	}

	var comment *models.Comment
	defer func() {
		// Check if issue admin/poster changes the status of issue.
		if (ctx.Repo.CanWriteIssuesOrPulls(issue.IsPull) || (ctx.IsSigned && issue.IsPoster(ctx.Doer.ID))) &&
			(form.Status == "reopen" || form.Status == "close") &&
			!(issue.IsPull && issue.PullRequest.HasMerged) {

			// Duplication and conflict check should apply to reopen pull request.
			var pr *models.PullRequest

			if form.Status == "reopen" && issue.IsPull {
				pull := issue.PullRequest
				var err error
				pr, err = models.GetUnmergedPullRequest(pull.HeadRepoID, pull.BaseRepoID, pull.HeadBranch, pull.BaseBranch, pull.Flow)
				if err != nil {
					if !models.IsErrPullRequestNotExist(err) {
						ctx.ServerError("GetUnmergedPullRequest", err)
						return
					}
				}

				// Regenerate patch and test conflict.
				if pr == nil {
					issue.PullRequest.HeadCommitID = ""
					pull_service.AddToTaskQueue(issue.PullRequest)
				}
			}

			if pr != nil {
				ctx.Flash.Info(ctx.Tr("repo.pulls.open_unmerged_pull_exists", pr.Index))
			} else {
				isClosed := form.Status == "close"
				if err := issue_service.ChangeStatus(issue, ctx.Doer, isClosed); err != nil {
					log.Error("ChangeStatus: %v", err)

					if models.IsErrDependenciesLeft(err) {
						if issue.IsPull {
							ctx.Flash.Error(ctx.Tr("repo.issues.dependency.pr_close_blocked"))
							ctx.Redirect(fmt.Sprintf("%s/pulls/%d", ctx.Repo.RepoLink, issue.Index))
						} else {
							ctx.Flash.Error(ctx.Tr("repo.issues.dependency.issue_close_blocked"))
							ctx.Redirect(fmt.Sprintf("%s/issues/%d", ctx.Repo.RepoLink, issue.Index))
						}
						return
					}
				} else {
					if err := stopTimerIfAvailable(ctx.Doer, issue); err != nil {
						ctx.ServerError("CreateOrStopIssueStopwatch", err)
						return
					}

					log.Trace("Issue [%d] status changed to closed: %v", issue.ID, issue.IsClosed)
				}
			}
		}

		// Redirect to comment hashtag if there is any actual content.
		typeName := "issues"
		if issue.IsPull {
			typeName = "pulls"
		}
		if comment != nil {
			ctx.Redirect(fmt.Sprintf("%s/%s/%d#%s", ctx.Repo.RepoLink, typeName, issue.Index, comment.HashTag()))
		} else {
			ctx.Redirect(fmt.Sprintf("%s/%s/%d", ctx.Repo.RepoLink, typeName, issue.Index))
		}
	}()

	// Fix #321: Allow empty comments, as long as we have attachments.
	if len(form.Content) == 0 && len(attachments) == 0 {
		return
	}

	comment, err := comment_service.CreateIssueComment(ctx.Doer, ctx.Repo.Repository, issue, form.Content, attachments)
	if err != nil {
		ctx.ServerError("CreateIssueComment", err)
		return
	}

	log.Trace("Comment created: %d/%d/%d", ctx.Repo.Repository.ID, issue.ID, comment.ID)
}

// UpdateCommentContent change comment of issue's content
func UpdateCommentContent(ctx *context.Context) {
	comment, err := models.GetCommentByID(ctx.ParamsInt64(":id"))
	if err != nil {
		ctx.NotFoundOrServerError("GetCommentByID", models.IsErrCommentNotExist, err)
		return
	}

	if err := comment.LoadIssue(); err != nil {
		ctx.NotFoundOrServerError("LoadIssue", models.IsErrIssueNotExist, err)
		return
	}

	if !ctx.IsSigned || (ctx.Doer.ID != comment.PosterID && !ctx.Repo.CanWriteIssuesOrPulls(comment.Issue.IsPull)) {
		ctx.Error(http.StatusForbidden)
		return
	}

	if comment.Type != models.CommentTypeComment && comment.Type != models.CommentTypeReview && comment.Type != models.CommentTypeCode {
		ctx.Error(http.StatusNoContent)
		return
	}

	oldContent := comment.Content
	comment.Content = ctx.FormString("content")
	if len(comment.Content) == 0 {
		ctx.JSON(http.StatusOK, map[string]interface{}{
			"content": "",
		})
		return
	}
	if err = comment_service.UpdateComment(comment, ctx.Doer, oldContent); err != nil {
		ctx.ServerError("UpdateComment", err)
		return
	}

	if err := comment.LoadAttachments(); err != nil {
		ctx.ServerError("LoadAttachments", err)
		return
	}

	// when the update request doesn't intend to update attachments (eg: change checkbox state), ignore attachment updates
	if !ctx.FormBool("ignore_attachments") {
		if err := updateAttachments(comment, ctx.FormStrings("files[]")); err != nil {
			ctx.ServerError("UpdateAttachments", err)
			return
		}
	}

	content, err := markdown.RenderString(&markup.RenderContext{
		URLPrefix: ctx.FormString("context"), // FIXME: <- IS THIS SAFE ?
		Metas:     ctx.Repo.Repository.ComposeMetas(),
		GitRepo:   ctx.Repo.GitRepo,
		Ctx:       ctx,
	}, comment.Content)
	if err != nil {
		ctx.ServerError("RenderString", err)
		return
	}

	ctx.JSON(http.StatusOK, map[string]interface{}{
		"content":     content,
		"attachments": attachmentsHTML(ctx, comment.Attachments, comment.Content),
	})
}

// DeleteComment delete comment of issue
func DeleteComment(ctx *context.Context) {
	comment, err := models.GetCommentByID(ctx.ParamsInt64(":id"))
	if err != nil {
		ctx.NotFoundOrServerError("GetCommentByID", models.IsErrCommentNotExist, err)
		return
	}

	if err := comment.LoadIssue(); err != nil {
		ctx.NotFoundOrServerError("LoadIssue", models.IsErrIssueNotExist, err)
		return
	}

	if !ctx.IsSigned || (ctx.Doer.ID != comment.PosterID && !ctx.Repo.CanWriteIssuesOrPulls(comment.Issue.IsPull)) {
		ctx.Error(http.StatusForbidden)
		return
	} else if comment.Type != models.CommentTypeComment && comment.Type != models.CommentTypeCode {
		ctx.Error(http.StatusNoContent)
		return
	}

	if err = comment_service.DeleteComment(ctx.Doer, comment); err != nil {
		ctx.ServerError("DeleteCommentByID", err)
		return
	}

	ctx.Status(http.StatusOK)
}

// ChangeIssueReaction create a reaction for issue
func ChangeIssueReaction(ctx *context.Context) {
	form := web.GetForm(ctx).(*forms.ReactionForm)
	issue := GetActionIssue(ctx)
	if ctx.Written() {
		return
	}

	if !ctx.IsSigned || (ctx.Doer.ID != issue.PosterID && !ctx.Repo.CanReadIssuesOrPulls(issue.IsPull)) {
		if log.IsTrace() {
			if ctx.IsSigned {
				issueType := "issues"
				if issue.IsPull {
					issueType = "pulls"
				}
				log.Trace("Permission Denied: User %-v not the Poster (ID: %d) and cannot read %s in Repo %-v.\n"+
					"User in Repo has Permissions: %-+v",
					ctx.Doer,
					log.NewColoredIDValue(issue.PosterID),
					issueType,
					ctx.Repo.Repository,
					ctx.Repo.Permission)
			} else {
				log.Trace("Permission Denied: Not logged in")
			}
		}

		ctx.Error(http.StatusForbidden)
		return
	}

	if ctx.HasError() {
		ctx.ServerError("ChangeIssueReaction", errors.New(ctx.GetErrMsg()))
		return
	}

	switch ctx.Params(":action") {
	case "react":
		reaction, err := issues_model.CreateIssueReaction(ctx.Doer.ID, issue.ID, form.Content)
		if err != nil {
			if issues_model.IsErrForbiddenIssueReaction(err) {
				ctx.ServerError("ChangeIssueReaction", err)
				return
			}
			log.Info("CreateIssueReaction: %s", err)
			break
		}
		// Reload new reactions
		issue.Reactions = nil
		if err = issue.LoadAttributes(); err != nil {
			log.Info("issue.LoadAttributes: %s", err)
			break
		}

		log.Trace("Reaction for issue created: %d/%d/%d", ctx.Repo.Repository.ID, issue.ID, reaction.ID)
	case "unreact":
		if err := issues_model.DeleteIssueReaction(ctx.Doer.ID, issue.ID, form.Content); err != nil {
			ctx.ServerError("DeleteIssueReaction", err)
			return
		}

		// Reload new reactions
		issue.Reactions = nil
		if err := issue.LoadAttributes(); err != nil {
			log.Info("issue.LoadAttributes: %s", err)
			break
		}

		log.Trace("Reaction for issue removed: %d/%d", ctx.Repo.Repository.ID, issue.ID)
	default:
		ctx.NotFound(fmt.Sprintf("Unknown action %s", ctx.Params(":action")), nil)
		return
	}

	if len(issue.Reactions) == 0 {
		ctx.JSON(http.StatusOK, map[string]interface{}{
			"empty": true,
			"html":  "",
		})
		return
	}

	html, err := ctx.RenderToString(tplReactions, map[string]interface{}{
		"ctx":       ctx.Data,
		"ActionURL": fmt.Sprintf("%s/issues/%d/reactions", ctx.Repo.RepoLink, issue.Index),
		"Reactions": issue.Reactions.GroupByType(),
	})
	if err != nil {
		ctx.ServerError("ChangeIssueReaction.HTMLString", err)
		return
	}
	ctx.JSON(http.StatusOK, map[string]interface{}{
		"html": html,
	})
}

// ChangeCommentReaction create a reaction for comment
func ChangeCommentReaction(ctx *context.Context) {
	form := web.GetForm(ctx).(*forms.ReactionForm)
	comment, err := models.GetCommentByID(ctx.ParamsInt64(":id"))
	if err != nil {
		ctx.NotFoundOrServerError("GetCommentByID", models.IsErrCommentNotExist, err)
		return
	}

	if err := comment.LoadIssue(); err != nil {
		ctx.NotFoundOrServerError("LoadIssue", models.IsErrIssueNotExist, err)
		return
	}

	if !ctx.IsSigned || (ctx.Doer.ID != comment.PosterID && !ctx.Repo.CanReadIssuesOrPulls(comment.Issue.IsPull)) {
		if log.IsTrace() {
			if ctx.IsSigned {
				issueType := "issues"
				if comment.Issue.IsPull {
					issueType = "pulls"
				}
				log.Trace("Permission Denied: User %-v not the Poster (ID: %d) and cannot read %s in Repo %-v.\n"+
					"User in Repo has Permissions: %-+v",
					ctx.Doer,
					log.NewColoredIDValue(comment.Issue.PosterID),
					issueType,
					ctx.Repo.Repository,
					ctx.Repo.Permission)
			} else {
				log.Trace("Permission Denied: Not logged in")
			}
		}

		ctx.Error(http.StatusForbidden)
		return
	}

	if comment.Type != models.CommentTypeComment && comment.Type != models.CommentTypeCode && comment.Type != models.CommentTypeReview {
		ctx.Error(http.StatusNoContent)
		return
	}

	switch ctx.Params(":action") {
	case "react":
		reaction, err := issues_model.CreateCommentReaction(ctx.Doer.ID, comment.Issue.ID, comment.ID, form.Content)
		if err != nil {
			if issues_model.IsErrForbiddenIssueReaction(err) {
				ctx.ServerError("ChangeIssueReaction", err)
				return
			}
			log.Info("CreateCommentReaction: %s", err)
			break
		}
		// Reload new reactions
		comment.Reactions = nil
		if err = comment.LoadReactions(ctx.Repo.Repository); err != nil {
			log.Info("comment.LoadReactions: %s", err)
			break
		}

		log.Trace("Reaction for comment created: %d/%d/%d/%d", ctx.Repo.Repository.ID, comment.Issue.ID, comment.ID, reaction.ID)
	case "unreact":
		if err := issues_model.DeleteCommentReaction(ctx.Doer.ID, comment.Issue.ID, comment.ID, form.Content); err != nil {
			ctx.ServerError("DeleteCommentReaction", err)
			return
		}

		// Reload new reactions
		comment.Reactions = nil
		if err = comment.LoadReactions(ctx.Repo.Repository); err != nil {
			log.Info("comment.LoadReactions: %s", err)
			break
		}

		log.Trace("Reaction for comment removed: %d/%d/%d", ctx.Repo.Repository.ID, comment.Issue.ID, comment.ID)
	default:
		ctx.NotFound(fmt.Sprintf("Unknown action %s", ctx.Params(":action")), nil)
		return
	}

	if len(comment.Reactions) == 0 {
		ctx.JSON(http.StatusOK, map[string]interface{}{
			"empty": true,
			"html":  "",
		})
		return
	}

	html, err := ctx.RenderToString(tplReactions, map[string]interface{}{
		"ctx":       ctx.Data,
		"ActionURL": fmt.Sprintf("%s/comments/%d/reactions", ctx.Repo.RepoLink, comment.ID),
		"Reactions": comment.Reactions.GroupByType(),
	})
	if err != nil {
		ctx.ServerError("ChangeCommentReaction.HTMLString", err)
		return
	}
	ctx.JSON(http.StatusOK, map[string]interface{}{
		"html": html,
	})
}

func addParticipant(poster *user_model.User, participants []*user_model.User) []*user_model.User {
	for _, part := range participants {
		if poster.ID == part.ID {
			return participants
		}
	}
	return append(participants, poster)
}

func filterXRefComments(ctx *context.Context, issue *models.Issue) error {
	// Remove comments that the user has no permissions to see
	for i := 0; i < len(issue.Comments); {
		c := issue.Comments[i]
		if models.CommentTypeIsRef(c.Type) && c.RefRepoID != issue.RepoID && c.RefRepoID != 0 {
			var err error
			// Set RefRepo for description in template
			c.RefRepo, err = repo_model.GetRepositoryByID(c.RefRepoID)
			if err != nil {
				return err
			}
			perm, err := models.GetUserRepoPermission(ctx, c.RefRepo, ctx.Doer)
			if err != nil {
				return err
			}
			if !perm.CanReadIssuesOrPulls(c.RefIsPull) {
				issue.Comments = append(issue.Comments[:i], issue.Comments[i+1:]...)
				continue
			}
		}
		i++
	}
	return nil
}

// GetIssueAttachments returns attachments for the issue
func GetIssueAttachments(ctx *context.Context) {
	issue := GetActionIssue(ctx)
	attachments := make([]*api.Attachment, len(issue.Attachments))
	for i := 0; i < len(issue.Attachments); i++ {
		attachments[i] = convert.ToReleaseAttachment(issue.Attachments[i])
	}
	ctx.JSON(http.StatusOK, attachments)
}

// GetCommentAttachments returns attachments for the comment
func GetCommentAttachments(ctx *context.Context) {
	comment, err := models.GetCommentByID(ctx.ParamsInt64(":id"))
	if err != nil {
		ctx.NotFoundOrServerError("GetCommentByID", models.IsErrCommentNotExist, err)
		return
	}
	attachments := make([]*api.Attachment, 0)
	if comment.Type == models.CommentTypeComment {
		if err := comment.LoadAttachments(); err != nil {
			ctx.ServerError("LoadAttachments", err)
			return
		}
		for i := 0; i < len(comment.Attachments); i++ {
			attachments = append(attachments, convert.ToReleaseAttachment(comment.Attachments[i]))
		}
	}
	ctx.JSON(http.StatusOK, attachments)
}

func updateAttachments(item interface{}, files []string) error {
	var attachments []*repo_model.Attachment
	switch content := item.(type) {
	case *models.Issue:
		attachments = content.Attachments
	case *models.Comment:
		attachments = content.Attachments
	default:
		return fmt.Errorf("unknown Type: %T", content)
	}
	for i := 0; i < len(attachments); i++ {
		if util.IsStringInSlice(attachments[i].UUID, files) {
			continue
		}
		if err := repo_model.DeleteAttachment(attachments[i], true); err != nil {
			return err
		}
	}
	var err error
	if len(files) > 0 {
		switch content := item.(type) {
		case *models.Issue:
			err = models.UpdateIssueAttachments(content.ID, files)
		case *models.Comment:
			err = content.UpdateAttachments(files)
		default:
			return fmt.Errorf("unknown Type: %T", content)
		}
		if err != nil {
			return err
		}
	}
	switch content := item.(type) {
	case *models.Issue:
		content.Attachments, err = repo_model.GetAttachmentsByIssueID(content.ID)
	case *models.Comment:
		content.Attachments, err = repo_model.GetAttachmentsByCommentID(content.ID)
	default:
		return fmt.Errorf("unknown Type: %T", content)
	}
	return err
}

func attachmentsHTML(ctx *context.Context, attachments []*repo_model.Attachment, content string) string {
	attachHTML, err := ctx.RenderToString(tplAttachment, map[string]interface{}{
		"ctx":         ctx.Data,
		"Attachments": attachments,
		"Content":     content,
	})
	if err != nil {
		ctx.ServerError("attachmentsHTML.HTMLString", err)
		return ""
	}
	return attachHTML
}

// combineLabelComments combine the nearby label comments as one.
func combineLabelComments(issue *models.Issue) {
	var prev, cur *models.Comment
	for i := 0; i < len(issue.Comments); i++ {
		cur = issue.Comments[i]
		if i > 0 {
			prev = issue.Comments[i-1]
		}
		if i == 0 || cur.Type != models.CommentTypeLabel ||
			(prev != nil && prev.PosterID != cur.PosterID) ||
			(prev != nil && cur.CreatedUnix-prev.CreatedUnix >= 60) {
			if cur.Type == models.CommentTypeLabel && cur.Label != nil {
				if cur.Content != "1" {
					cur.RemovedLabels = append(cur.RemovedLabels, cur.Label)
				} else {
					cur.AddedLabels = append(cur.AddedLabels, cur.Label)
				}
			}
			continue
		}

		if cur.Label != nil { // now cur MUST be label comment
			if prev.Type == models.CommentTypeLabel { // we can combine them only prev is a label comment
				if cur.Content != "1" {
					// remove labels from the AddedLabels list if the label that was removed is already
					// in this list, and if it's not in this list, add the label to RemovedLabels
					addedAndRemoved := false
					for i, label := range prev.AddedLabels {
						if cur.Label.ID == label.ID {
							prev.AddedLabels = append(prev.AddedLabels[:i], prev.AddedLabels[i+1:]...)
							addedAndRemoved = true
							break
						}
					}
					if !addedAndRemoved {
						prev.RemovedLabels = append(prev.RemovedLabels, cur.Label)
					}
				} else {
					// remove labels from the RemovedLabels list if the label that was added is already
					// in this list, and if it's not in this list, add the label to AddedLabels
					removedAndAdded := false
					for i, label := range prev.RemovedLabels {
						if cur.Label.ID == label.ID {
							prev.RemovedLabels = append(prev.RemovedLabels[:i], prev.RemovedLabels[i+1:]...)
							removedAndAdded = true
							break
						}
					}
					if !removedAndAdded {
						prev.AddedLabels = append(prev.AddedLabels, cur.Label)
					}
				}
				prev.CreatedUnix = cur.CreatedUnix
				// remove the current comment since it has been combined to prev comment
				issue.Comments = append(issue.Comments[:i], issue.Comments[i+1:]...)
				i--
			} else { // if prev is not a label comment, start a new group
				if cur.Content != "1" {
					cur.RemovedLabels = append(cur.RemovedLabels, cur.Label)
				} else {
					cur.AddedLabels = append(cur.AddedLabels, cur.Label)
				}
			}
		}
	}
}

// get all teams that current user can mention
func handleTeamMentions(ctx *context.Context) {
	if ctx.Doer == nil || !ctx.Repo.Owner.IsOrganization() {
		return
	}

	var isAdmin bool
	var err error
	var teams []*organization.Team
	org := organization.OrgFromUser(ctx.Repo.Owner)
	// Admin has super access.
	if ctx.Doer.IsAdmin {
		isAdmin = true
	} else {
		isAdmin, err = org.IsOwnedBy(ctx.Doer.ID)
		if err != nil {
			ctx.ServerError("IsOwnedBy", err)
			return
		}
	}

	if isAdmin {
		teams, err = org.LoadTeams()
		if err != nil {
			ctx.ServerError("LoadTeams", err)
			return
		}
	} else {
		teams, err = org.GetUserTeams(ctx.Doer.ID)
		if err != nil {
			ctx.ServerError("GetUserTeams", err)
			return
		}
	}

	ctx.Data["MentionableTeams"] = teams
	ctx.Data["MentionableTeamsOrg"] = ctx.Repo.Owner.Name
	ctx.Data["MentionableTeamsOrgAvatar"] = ctx.Repo.Owner.AvatarLink()
}
