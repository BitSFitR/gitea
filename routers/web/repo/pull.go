// Copyright 2018 The Gitea Authors.
// Copyright 2014 The Gogs Authors.
// All rights reserved.
// Use of this source code is governed by a MIT-style
// license that can be found in the LICENSE file.

package repo

import (
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"code.gitea.io/gitea/models"
	"code.gitea.io/gitea/models/db"
	"code.gitea.io/gitea/models/organization"
	repo_model "code.gitea.io/gitea/models/repo"
	"code.gitea.io/gitea/models/unit"
	user_model "code.gitea.io/gitea/models/user"
	"code.gitea.io/gitea/modules/base"
	"code.gitea.io/gitea/modules/context"
	"code.gitea.io/gitea/modules/git"
	"code.gitea.io/gitea/modules/log"
	"code.gitea.io/gitea/modules/notification"
	"code.gitea.io/gitea/modules/setting"
	"code.gitea.io/gitea/modules/structs"
	"code.gitea.io/gitea/modules/upload"
	"code.gitea.io/gitea/modules/util"
	"code.gitea.io/gitea/modules/web"
	"code.gitea.io/gitea/modules/web/middleware"
	"code.gitea.io/gitea/routers/utils"
	asymkey_service "code.gitea.io/gitea/services/asymkey"
	"code.gitea.io/gitea/services/forms"
	"code.gitea.io/gitea/services/gitdiff"
	pull_service "code.gitea.io/gitea/services/pull"
	repo_service "code.gitea.io/gitea/services/repository"
)

const (
	tplFork        base.TplName = "repo/pulls/fork"
	tplCompareDiff base.TplName = "repo/diff/compare"
	tplPullCommits base.TplName = "repo/pulls/commits"
	tplPullFiles   base.TplName = "repo/pulls/files"

	pullRequestTemplateKey = "PullRequestTemplate"
)

var pullRequestTemplateCandidates = []string{
	"PULL_REQUEST_TEMPLATE.md",
	"pull_request_template.md",
	".gitea/PULL_REQUEST_TEMPLATE.md",
	".gitea/pull_request_template.md",
	".github/PULL_REQUEST_TEMPLATE.md",
	".github/pull_request_template.md",
}

func getRepository(ctx *context.Context, repoID int64) *repo_model.Repository {
	repo, err := repo_model.GetRepositoryByID(repoID)
	if err != nil {
		if repo_model.IsErrRepoNotExist(err) {
			ctx.NotFound("GetRepositoryByID", nil)
		} else {
			ctx.ServerError("GetRepositoryByID", err)
		}
		return nil
	}

	perm, err := models.GetUserRepoPermission(ctx, repo, ctx.Doer)
	if err != nil {
		ctx.ServerError("GetUserRepoPermission", err)
		return nil
	}

	if !perm.CanRead(unit.TypeCode) {
		log.Trace("Permission Denied: User %-v cannot read %-v of repo %-v\n"+
			"User in repo has Permissions: %-+v",
			ctx.Doer,
			unit.TypeCode,
			ctx.Repo,
			perm)
		ctx.NotFound("getRepository", nil)
		return nil
	}
	return repo
}

func getForkRepository(ctx *context.Context) *repo_model.Repository {
	forkRepo := getRepository(ctx, ctx.ParamsInt64(":repoid"))
	if ctx.Written() {
		return nil
	}

	if forkRepo.IsEmpty {
		log.Trace("Empty repository %-v", forkRepo)
		ctx.NotFound("getForkRepository", nil)
		return nil
	}

	if err := forkRepo.GetOwner(ctx); err != nil {
		ctx.ServerError("GetOwner", err)
		return nil
	}

	ctx.Data["repo_name"] = forkRepo.Name
	ctx.Data["description"] = forkRepo.Description
	ctx.Data["IsPrivate"] = forkRepo.IsPrivate || forkRepo.Owner.Visibility == structs.VisibleTypePrivate
	canForkToUser := forkRepo.OwnerID != ctx.Doer.ID && !repo_model.HasForkedRepo(ctx.Doer.ID, forkRepo.ID)

	ctx.Data["ForkRepo"] = forkRepo

	ownedOrgs, err := organization.GetOrgsCanCreateRepoByUserID(ctx.Doer.ID)
	if err != nil {
		ctx.ServerError("GetOrgsCanCreateRepoByUserID", err)
		return nil
	}
	var orgs []*organization.Organization
	for _, org := range ownedOrgs {
		if forkRepo.OwnerID != org.ID && !repo_model.HasForkedRepo(org.ID, forkRepo.ID) {
			orgs = append(orgs, org)
		}
	}

	traverseParentRepo := forkRepo
	for {
		if ctx.Doer.ID == traverseParentRepo.OwnerID {
			canForkToUser = false
		} else {
			for i, org := range orgs {
				if org.ID == traverseParentRepo.OwnerID {
					orgs = append(orgs[:i], orgs[i+1:]...)
					break
				}
			}
		}

		if !traverseParentRepo.IsFork {
			break
		}
		traverseParentRepo, err = repo_model.GetRepositoryByID(traverseParentRepo.ForkID)
		if err != nil {
			ctx.ServerError("GetRepositoryByID", err)
			return nil
		}
	}

	ctx.Data["CanForkToUser"] = canForkToUser
	ctx.Data["Orgs"] = orgs

	if canForkToUser {
		ctx.Data["ContextUser"] = ctx.Doer
	} else if len(orgs) > 0 {
		ctx.Data["ContextUser"] = orgs[0]
	}

	return forkRepo
}

// Fork render repository fork page
func Fork(ctx *context.Context) {
	ctx.Data["Title"] = ctx.Tr("new_fork")

	getForkRepository(ctx)
	if ctx.Written() {
		return
	}

	ctx.HTML(http.StatusOK, tplFork)
}

// ForkPost response for forking a repository
func ForkPost(ctx *context.Context) {
	form := web.GetForm(ctx).(*forms.CreateRepoForm)
	ctx.Data["Title"] = ctx.Tr("new_fork")

	ctxUser := checkContextUser(ctx, form.UID)
	if ctx.Written() {
		return
	}

	forkRepo := getForkRepository(ctx)
	if ctx.Written() {
		return
	}

	ctx.Data["ContextUser"] = ctxUser

	if ctx.HasError() {
		ctx.HTML(http.StatusOK, tplFork)
		return
	}

	var err error
	traverseParentRepo := forkRepo
	for {
		if ctxUser.ID == traverseParentRepo.OwnerID {
			ctx.RenderWithErr(ctx.Tr("repo.settings.new_owner_has_same_repo"), tplFork, &form)
			return
		}
		repo := repo_model.GetForkedRepo(ctxUser.ID, traverseParentRepo.ID)
		if repo != nil {
			ctx.Redirect(ctxUser.HomeLink() + "/" + url.PathEscape(repo.Name))
			return
		}
		if !traverseParentRepo.IsFork {
			break
		}
		traverseParentRepo, err = repo_model.GetRepositoryByID(traverseParentRepo.ForkID)
		if err != nil {
			ctx.ServerError("GetRepositoryByID", err)
			return
		}
	}

	// Check if user is allowed to create repo's on the organization.
	if ctxUser.IsOrganization() {
		isAllowedToFork, err := organization.OrgFromUser(ctxUser).CanCreateOrgRepo(ctx.Doer.ID)
		if err != nil {
			ctx.ServerError("CanCreateOrgRepo", err)
			return
		} else if !isAllowedToFork {
			ctx.Error(http.StatusForbidden)
			return
		}
	}

	repo, err := repo_service.ForkRepository(ctx, ctx.Doer, ctxUser, repo_service.ForkRepoOptions{
		BaseRepo:    forkRepo,
		Name:        form.RepoName,
		Description: form.Description,
	})
	if err != nil {
		ctx.Data["Err_RepoName"] = true
		switch {
		case repo_model.IsErrRepoAlreadyExist(err):
			ctx.RenderWithErr(ctx.Tr("repo.settings.new_owner_has_same_repo"), tplFork, &form)
		case db.IsErrNameReserved(err):
			ctx.RenderWithErr(ctx.Tr("repo.form.name_reserved", err.(db.ErrNameReserved).Name), tplFork, &form)
		case db.IsErrNamePatternNotAllowed(err):
			ctx.RenderWithErr(ctx.Tr("repo.form.name_pattern_not_allowed", err.(db.ErrNamePatternNotAllowed).Pattern), tplFork, &form)
		default:
			ctx.ServerError("ForkPost", err)
		}
		return
	}

	log.Trace("Repository forked[%d]: %s/%s", forkRepo.ID, ctxUser.Name, repo.Name)
	ctx.Redirect(ctxUser.HomeLink() + "/" + url.PathEscape(repo.Name))
}

func checkPullInfo(ctx *context.Context) *models.Issue {
	issue, err := models.GetIssueByIndex(ctx.Repo.Repository.ID, ctx.ParamsInt64(":index"))
	if err != nil {
		if models.IsErrIssueNotExist(err) {
			ctx.NotFound("GetIssueByIndex", err)
		} else {
			ctx.ServerError("GetIssueByIndex", err)
		}
		return nil
	}
	if err = issue.LoadPoster(); err != nil {
		ctx.ServerError("LoadPoster", err)
		return nil
	}
	if err := issue.LoadRepo(ctx); err != nil {
		ctx.ServerError("LoadRepo", err)
		return nil
	}
	ctx.Data["Title"] = fmt.Sprintf("#%d - %s", issue.Index, issue.Title)
	ctx.Data["Issue"] = issue

	if !issue.IsPull {
		ctx.NotFound("ViewPullCommits", nil)
		return nil
	}

	if err = issue.LoadPullRequest(); err != nil {
		ctx.ServerError("LoadPullRequest", err)
		return nil
	}

	if err = issue.PullRequest.LoadHeadRepoCtx(ctx); err != nil {
		ctx.ServerError("LoadHeadRepo", err)
		return nil
	}

	if ctx.IsSigned {
		// Update issue-user.
		if err = issue.ReadBy(ctx, ctx.Doer.ID); err != nil {
			ctx.ServerError("ReadBy", err)
			return nil
		}
	}

	return issue
}

func setMergeTarget(ctx *context.Context, pull *models.PullRequest) {
	if ctx.Repo.Owner.Name == pull.MustHeadUserName() {
		ctx.Data["HeadTarget"] = pull.HeadBranch
	} else if pull.HeadRepo == nil {
		ctx.Data["HeadTarget"] = pull.MustHeadUserName() + ":" + pull.HeadBranch
	} else {
		ctx.Data["HeadTarget"] = pull.MustHeadUserName() + "/" + pull.HeadRepo.Name + ":" + pull.HeadBranch
	}
	ctx.Data["BaseTarget"] = pull.BaseBranch
	ctx.Data["HeadBranchHTMLURL"] = pull.GetHeadBranchHTMLURL()
	ctx.Data["BaseBranchHTMLURL"] = pull.GetBaseBranchHTMLURL()
}

// PrepareMergedViewPullInfo show meta information for a merged pull request view page
func PrepareMergedViewPullInfo(ctx *context.Context, issue *models.Issue) *git.CompareInfo {
	pull := issue.PullRequest

	setMergeTarget(ctx, pull)
	ctx.Data["HasMerged"] = true

	var baseCommit string
	// Some migrated PR won't have any Base SHA and lose history, try to get one
	if pull.MergeBase == "" {
		var commitSHA, parentCommit string
		// If there is a head or a patch file, and it is readable, grab info
		commitSHA, err := ctx.Repo.GitRepo.GetRefCommitID(pull.GetGitRefName())
		if err != nil {
			// Head File does not exist, try the patch
			commitSHA, err = ctx.Repo.GitRepo.ReadPatchCommit(pull.Index)
			if err == nil {
				// Recreate pull head in files for next time
				if err := ctx.Repo.GitRepo.SetReference(pull.GetGitRefName(), commitSHA); err != nil {
					log.Error("Could not write head file", err)
				}
			} else {
				// There is no history available
				log.Trace("No history file available for PR %d", pull.Index)
			}
		}
		if commitSHA != "" {
			// Get immediate parent of the first commit in the patch, grab history back
			parentCommit, _, err = git.NewCommand(ctx, "rev-list", "-1", "--skip=1", commitSHA).RunStdString(&git.RunOpts{Dir: ctx.Repo.GitRepo.Path})
			if err == nil {
				parentCommit = strings.TrimSpace(parentCommit)
			}
			// Special case on Git < 2.25 that doesn't fail on immediate empty history
			if err != nil || parentCommit == "" {
				log.Info("No known parent commit for PR %d, error: %v", pull.Index, err)
				// bring at least partial history if it can work
				parentCommit = commitSHA
			}
		}
		baseCommit = parentCommit
	} else {
		// Keep an empty history or original commit
		baseCommit = pull.MergeBase
	}

	compareInfo, err := ctx.Repo.GitRepo.GetCompareInfo(ctx.Repo.Repository.RepoPath(),
		baseCommit, pull.GetGitRefName(), false, false)
	if err != nil {
		if strings.Contains(err.Error(), "fatal: Not a valid object name") || strings.Contains(err.Error(), "unknown revision or path not in the working tree") {
			ctx.Data["IsPullRequestBroken"] = true
			ctx.Data["BaseTarget"] = pull.BaseBranch
			ctx.Data["NumCommits"] = 0
			ctx.Data["NumFiles"] = 0
			return nil
		}

		ctx.ServerError("GetCompareInfo", err)
		return nil
	}
	ctx.Data["NumCommits"] = len(compareInfo.Commits)
	ctx.Data["NumFiles"] = compareInfo.NumFiles

	if len(compareInfo.Commits) != 0 {
		sha := compareInfo.Commits[0].ID.String()
		commitStatuses, _, err := models.GetLatestCommitStatus(ctx.Repo.Repository.ID, sha, db.ListOptions{})
		if err != nil {
			ctx.ServerError("GetLatestCommitStatus", err)
			return nil
		}
		if len(commitStatuses) != 0 {
			ctx.Data["LatestCommitStatuses"] = commitStatuses
			ctx.Data["LatestCommitStatus"] = models.CalcCommitStatus(commitStatuses)
		}
	}

	return compareInfo
}

// PrepareViewPullInfo show meta information for a pull request preview page
func PrepareViewPullInfo(ctx *context.Context, issue *models.Issue) *git.CompareInfo {
	ctx.Data["PullRequestWorkInProgressPrefixes"] = setting.Repository.PullRequest.WorkInProgressPrefixes

	repo := ctx.Repo.Repository
	pull := issue.PullRequest

	if err := pull.LoadHeadRepoCtx(ctx); err != nil {
		ctx.ServerError("LoadHeadRepo", err)
		return nil
	}

	if err := pull.LoadBaseRepoCtx(ctx); err != nil {
		ctx.ServerError("LoadBaseRepo", err)
		return nil
	}

	setMergeTarget(ctx, pull)

	if err := pull.LoadProtectedBranch(); err != nil {
		ctx.ServerError("LoadProtectedBranch", err)
		return nil
	}
	ctx.Data["EnableStatusCheck"] = pull.ProtectedBranch != nil && pull.ProtectedBranch.EnableStatusCheck

	var baseGitRepo *git.Repository
	if pull.BaseRepoID == ctx.Repo.Repository.ID && ctx.Repo.GitRepo != nil {
		baseGitRepo = ctx.Repo.GitRepo
	} else {
		baseGitRepo, err := git.OpenRepository(ctx, pull.BaseRepo.RepoPath())
		if err != nil {
			ctx.ServerError("OpenRepository", err)
			return nil
		}
		defer baseGitRepo.Close()
	}

	if !baseGitRepo.IsBranchExist(pull.BaseBranch) {
		ctx.Data["IsPullRequestBroken"] = true
		ctx.Data["BaseTarget"] = pull.BaseBranch
		ctx.Data["HeadTarget"] = pull.HeadBranch

		sha, err := baseGitRepo.GetRefCommitID(pull.GetGitRefName())
		if err != nil {
			ctx.ServerError(fmt.Sprintf("GetRefCommitID(%s)", pull.GetGitRefName()), err)
			return nil
		}
		commitStatuses, _, err := models.GetLatestCommitStatus(repo.ID, sha, db.ListOptions{})
		if err != nil {
			ctx.ServerError("GetLatestCommitStatus", err)
			return nil
		}
		if len(commitStatuses) > 0 {
			ctx.Data["LatestCommitStatuses"] = commitStatuses
			ctx.Data["LatestCommitStatus"] = models.CalcCommitStatus(commitStatuses)
		}

		compareInfo, err := baseGitRepo.GetCompareInfo(pull.BaseRepo.RepoPath(),
			pull.MergeBase, pull.GetGitRefName(), false, false)
		if err != nil {
			if strings.Contains(err.Error(), "fatal: Not a valid object name") {
				ctx.Data["IsPullRequestBroken"] = true
				ctx.Data["BaseTarget"] = pull.BaseBranch
				ctx.Data["NumCommits"] = 0
				ctx.Data["NumFiles"] = 0
				return nil
			}

			ctx.ServerError("GetCompareInfo", err)
			return nil
		}

		ctx.Data["NumCommits"] = len(compareInfo.Commits)
		ctx.Data["NumFiles"] = compareInfo.NumFiles
		return compareInfo
	}

	var headBranchExist bool
	var headBranchSha string
	// HeadRepo may be missing
	if pull.HeadRepo != nil {
		headGitRepo, err := git.OpenRepository(ctx, pull.HeadRepo.RepoPath())
		if err != nil {
			ctx.ServerError("OpenRepository", err)
			return nil
		}
		defer headGitRepo.Close()

		if pull.Flow == models.PullRequestFlowGithub {
			headBranchExist = headGitRepo.IsBranchExist(pull.HeadBranch)
		} else {
			headBranchExist = git.IsReferenceExist(ctx, baseGitRepo.Path, pull.GetGitRefName())
		}

		if headBranchExist {
			if pull.Flow != models.PullRequestFlowGithub {
				headBranchSha, err = baseGitRepo.GetRefCommitID(pull.GetGitRefName())
			} else {
				headBranchSha, err = headGitRepo.GetBranchCommitID(pull.HeadBranch)
			}
			if err != nil {
				ctx.ServerError("GetBranchCommitID", err)
				return nil
			}
		}
	}

	if headBranchExist {
		var err error
		ctx.Data["UpdateAllowed"], ctx.Data["UpdateByRebaseAllowed"], err = pull_service.IsUserAllowedToUpdate(ctx, pull, ctx.Doer)
		if err != nil {
			ctx.ServerError("IsUserAllowedToUpdate", err)
			return nil
		}
		ctx.Data["GetCommitMessages"] = pull_service.GetSquashMergeCommitMessages(ctx, pull)
	}

	sha, err := baseGitRepo.GetRefCommitID(pull.GetGitRefName())
	if err != nil {
		if git.IsErrNotExist(err) {
			ctx.Data["IsPullRequestBroken"] = true
			if pull.IsSameRepo() {
				ctx.Data["HeadTarget"] = pull.HeadBranch
			} else if pull.HeadRepo == nil {
				ctx.Data["HeadTarget"] = "<deleted>:" + pull.HeadBranch
			} else {
				ctx.Data["HeadTarget"] = pull.HeadRepo.OwnerName + ":" + pull.HeadBranch
			}
			ctx.Data["BaseTarget"] = pull.BaseBranch
			ctx.Data["NumCommits"] = 0
			ctx.Data["NumFiles"] = 0
			return nil
		}
		ctx.ServerError(fmt.Sprintf("GetRefCommitID(%s)", pull.GetGitRefName()), err)
		return nil
	}

	commitStatuses, _, err := models.GetLatestCommitStatus(repo.ID, sha, db.ListOptions{})
	if err != nil {
		ctx.ServerError("GetLatestCommitStatus", err)
		return nil
	}
	if len(commitStatuses) > 0 {
		ctx.Data["LatestCommitStatuses"] = commitStatuses
		ctx.Data["LatestCommitStatus"] = models.CalcCommitStatus(commitStatuses)
	}

	if pull.ProtectedBranch != nil && pull.ProtectedBranch.EnableStatusCheck {
		ctx.Data["is_context_required"] = func(context string) bool {
			for _, c := range pull.ProtectedBranch.StatusCheckContexts {
				if c == context {
					return true
				}
			}
			return false
		}
		ctx.Data["RequiredStatusCheckState"] = pull_service.MergeRequiredContextsCommitStatus(commitStatuses, pull.ProtectedBranch.StatusCheckContexts)
	}

	ctx.Data["HeadBranchMovedOn"] = headBranchSha != sha
	ctx.Data["HeadBranchCommitID"] = headBranchSha
	ctx.Data["PullHeadCommitID"] = sha

	if pull.HeadRepo == nil || !headBranchExist || headBranchSha != sha {
		ctx.Data["IsPullRequestBroken"] = true
		if pull.IsSameRepo() {
			ctx.Data["HeadTarget"] = pull.HeadBranch
		} else if pull.HeadRepo == nil {
			ctx.Data["HeadTarget"] = "<deleted>:" + pull.HeadBranch
		} else {
			ctx.Data["HeadTarget"] = pull.HeadRepo.OwnerName + ":" + pull.HeadBranch
		}
	}

	compareInfo, err := baseGitRepo.GetCompareInfo(pull.BaseRepo.RepoPath(),
		git.BranchPrefix+pull.BaseBranch, pull.GetGitRefName(), false, false)
	if err != nil {
		if strings.Contains(err.Error(), "fatal: Not a valid object name") {
			ctx.Data["IsPullRequestBroken"] = true
			ctx.Data["BaseTarget"] = pull.BaseBranch
			ctx.Data["NumCommits"] = 0
			ctx.Data["NumFiles"] = 0
			return nil
		}

		ctx.ServerError("GetCompareInfo", err)
		return nil
	}

	if compareInfo.HeadCommitID == compareInfo.MergeBase {
		ctx.Data["IsNothingToCompare"] = true
	}

	if pull.IsWorkInProgress() {
		ctx.Data["IsPullWorkInProgress"] = true
		ctx.Data["WorkInProgressPrefix"] = pull.GetWorkInProgressPrefix()
	}

	if pull.IsFilesConflicted() {
		ctx.Data["IsPullFilesConflicted"] = true
		ctx.Data["ConflictedFiles"] = pull.ConflictedFiles
	}

	ctx.Data["NumCommits"] = len(compareInfo.Commits)
	ctx.Data["NumFiles"] = compareInfo.NumFiles
	return compareInfo
}

// ViewPullCommits show commits for a pull request
func ViewPullCommits(ctx *context.Context) {
	ctx.Data["PageIsPullList"] = true
	ctx.Data["PageIsPullCommits"] = true

	issue := checkPullInfo(ctx)
	if ctx.Written() {
		return
	}
	pull := issue.PullRequest

	var prInfo *git.CompareInfo
	if pull.HasMerged {
		prInfo = PrepareMergedViewPullInfo(ctx, issue)
	} else {
		prInfo = PrepareViewPullInfo(ctx, issue)
	}

	if ctx.Written() {
		return
	} else if prInfo == nil {
		ctx.NotFound("ViewPullCommits", nil)
		return
	}

	ctx.Data["Username"] = ctx.Repo.Owner.Name
	ctx.Data["Reponame"] = ctx.Repo.Repository.Name

	commits := models.ConvertFromGitCommit(prInfo.Commits, ctx.Repo.Repository)
	ctx.Data["Commits"] = commits
	ctx.Data["CommitCount"] = len(commits)

	getBranchData(ctx, issue)
	ctx.HTML(http.StatusOK, tplPullCommits)
}

// ViewPullFiles render pull request changed files list page
func ViewPullFiles(ctx *context.Context) {
	ctx.Data["PageIsPullList"] = true
	ctx.Data["PageIsPullFiles"] = true

	issue := checkPullInfo(ctx)
	if ctx.Written() {
		return
	}
	pull := issue.PullRequest

	var (
		startCommitID string
		endCommitID   string
		gitRepo       = ctx.Repo.GitRepo
	)

	var prInfo *git.CompareInfo
	if pull.HasMerged {
		prInfo = PrepareMergedViewPullInfo(ctx, issue)
	} else {
		prInfo = PrepareViewPullInfo(ctx, issue)
	}

	if ctx.Written() {
		return
	} else if prInfo == nil {
		ctx.NotFound("ViewPullFiles", nil)
		return
	}

	headCommitID, err := gitRepo.GetRefCommitID(pull.GetGitRefName())
	if err != nil {
		ctx.ServerError("GetRefCommitID", err)
		return
	}

	startCommitID = prInfo.MergeBase
	endCommitID = headCommitID

	ctx.Data["Username"] = ctx.Repo.Owner.Name
	ctx.Data["Reponame"] = ctx.Repo.Repository.Name
	ctx.Data["AfterCommitID"] = endCommitID

	fileOnly := ctx.FormBool("file-only")

	maxLines, maxFiles := setting.Git.MaxGitDiffLines, setting.Git.MaxGitDiffFiles
	files := ctx.FormStrings("files")
	if fileOnly && (len(files) == 2 || len(files) == 1) {
		maxLines, maxFiles = -1, -1
	}
	diffOptions := &gitdiff.DiffOptions{
		BeforeCommitID:     startCommitID,
		AfterCommitID:      endCommitID,
		SkipTo:             ctx.FormString("skip-to"),
		MaxLines:           maxLines,
		MaxLineCharacters:  setting.Git.MaxGitDiffLineCharacters,
		MaxFiles:           maxFiles,
		WhitespaceBehavior: gitdiff.GetWhitespaceFlag(ctx.Data["WhitespaceBehavior"].(string)),
	}

	var methodWithError string
	var diff *gitdiff.Diff
	if !ctx.IsSigned {
		diff, err = gitdiff.GetDiff(gitRepo, diffOptions, files...)
		methodWithError = "GetDiff"
	} else {
		diff, err = gitdiff.SyncAndGetUserSpecificDiff(ctx, ctx.Doer.ID, pull, gitRepo, diffOptions, files...)
		methodWithError = "SyncAndGetUserSpecificDiff"
	}
	if err != nil {
		ctx.ServerError(methodWithError, err)
		return
	}

	ctx.PageData["prReview"] = map[string]interface{}{
		"numberOfFiles":       diff.NumFiles,
		"numberOfViewedFiles": diff.NumViewedFiles,
	}

	if err = diff.LoadComments(ctx, issue, ctx.Doer); err != nil {
		ctx.ServerError("LoadComments", err)
		return
	}

	if err = pull.LoadProtectedBranch(); err != nil {
		ctx.ServerError("LoadProtectedBranch", err)
		return
	}

	if pull.ProtectedBranch != nil {
		glob := pull.ProtectedBranch.GetProtectedFilePatterns()
		if len(glob) != 0 {
			for _, file := range diff.Files {
				file.IsProtected = pull.ProtectedBranch.IsProtectedFile(glob, file.Name)
			}
		}
	}

	ctx.Data["Diff"] = diff
	ctx.Data["DiffNotAvailable"] = diff.NumFiles == 0

	baseCommit, err := ctx.Repo.GitRepo.GetCommit(startCommitID)
	if err != nil {
		ctx.ServerError("GetCommit", err)
		return
	}
	commit, err := gitRepo.GetCommit(endCommitID)
	if err != nil {
		ctx.ServerError("GetCommit", err)
		return
	}

	if ctx.IsSigned && ctx.Doer != nil {
		if ctx.Data["CanMarkConversation"], err = models.CanMarkConversation(issue, ctx.Doer); err != nil {
			ctx.ServerError("CanMarkConversation", err)
			return
		}
	}

	setCompareContext(ctx, baseCommit, commit, ctx.Repo.Owner.Name, ctx.Repo.Repository.Name)

	ctx.Data["RequireTribute"] = true
	if ctx.Data["Assignees"], err = models.GetRepoAssignees(ctx.Repo.Repository); err != nil {
		ctx.ServerError("GetAssignees", err)
		return
	}
	handleTeamMentions(ctx)
	if ctx.Written() {
		return
	}

	currentReview, err := models.GetCurrentReview(ctx.Doer, issue)
	if err != nil && !models.IsErrReviewNotExist(err) {
		ctx.ServerError("GetCurrentReview", err)
		return
	}
	numPendingCodeComments := int64(0)
	if currentReview != nil {
		numPendingCodeComments, err = models.CountComments(&models.FindCommentsOptions{
			Type:     models.CommentTypeCode,
			ReviewID: currentReview.ID,
			IssueID:  issue.ID,
		})
		if err != nil {
			ctx.ServerError("CountComments", err)
			return
		}
	}
	ctx.Data["CurrentReview"] = currentReview
	ctx.Data["PendingCodeCommentNumber"] = numPendingCodeComments

	getBranchData(ctx, issue)
	ctx.Data["IsIssuePoster"] = ctx.IsSigned && issue.IsPoster(ctx.Doer.ID)
	ctx.Data["HasIssuesOrPullsWritePermission"] = ctx.Repo.CanWriteIssuesOrPulls(issue.IsPull)

	ctx.Data["IsAttachmentEnabled"] = setting.Attachment.Enabled
	upload.AddUploadContext(ctx, "comment")

	ctx.HTML(http.StatusOK, tplPullFiles)
}

// UpdatePullRequest merge PR's baseBranch into headBranch
func UpdatePullRequest(ctx *context.Context) {
	issue := checkPullInfo(ctx)
	if ctx.Written() {
		return
	}
	if issue.IsClosed {
		ctx.NotFound("MergePullRequest", nil)
		return
	}
	if issue.PullRequest.HasMerged {
		ctx.NotFound("MergePullRequest", nil)
		return
	}

	rebase := ctx.FormString("style") == "rebase"

	if err := issue.PullRequest.LoadBaseRepoCtx(ctx); err != nil {
		ctx.ServerError("LoadBaseRepo", err)
		return
	}
	if err := issue.PullRequest.LoadHeadRepoCtx(ctx); err != nil {
		ctx.ServerError("LoadHeadRepo", err)
		return
	}

	allowedUpdateByMerge, allowedUpdateByRebase, err := pull_service.IsUserAllowedToUpdate(ctx, issue.PullRequest, ctx.Doer)
	if err != nil {
		ctx.ServerError("IsUserAllowedToMerge", err)
		return
	}

	// ToDo: add check if maintainers are allowed to change branch ... (need migration & co)
	if (!allowedUpdateByMerge && !rebase) || (rebase && !allowedUpdateByRebase) {
		ctx.Flash.Error(ctx.Tr("repo.pulls.update_not_allowed"))
		ctx.Redirect(issue.Link())
		return
	}

	// default merge commit message
	message := fmt.Sprintf("Merge branch '%s' into %s", issue.PullRequest.BaseBranch, issue.PullRequest.HeadBranch)

	if err = pull_service.Update(ctx, issue.PullRequest, ctx.Doer, message, rebase); err != nil {
		if models.IsErrMergeConflicts(err) {
			conflictError := err.(models.ErrMergeConflicts)
			flashError, err := ctx.RenderToString(tplAlertDetails, map[string]interface{}{
				"Message": ctx.Tr("repo.pulls.merge_conflict"),
				"Summary": ctx.Tr("repo.pulls.merge_conflict_summary"),
				"Details": utils.SanitizeFlashErrorString(conflictError.StdErr) + "<br>" + utils.SanitizeFlashErrorString(conflictError.StdOut),
			})
			if err != nil {
				ctx.ServerError("UpdatePullRequest.HTMLString", err)
				return
			}
			ctx.Flash.Error(flashError)
			ctx.Redirect(issue.Link())
			return
		} else if models.IsErrRebaseConflicts(err) {
			conflictError := err.(models.ErrRebaseConflicts)
			flashError, err := ctx.RenderToString(tplAlertDetails, map[string]interface{}{
				"Message": ctx.Tr("repo.pulls.rebase_conflict", utils.SanitizeFlashErrorString(conflictError.CommitSHA)),
				"Summary": ctx.Tr("repo.pulls.rebase_conflict_summary"),
				"Details": utils.SanitizeFlashErrorString(conflictError.StdErr) + "<br>" + utils.SanitizeFlashErrorString(conflictError.StdOut),
			})
			if err != nil {
				ctx.ServerError("UpdatePullRequest.HTMLString", err)
				return
			}
			ctx.Flash.Error(flashError)
			ctx.Redirect(issue.Link())
			return

		}
		ctx.Flash.Error(err.Error())
		ctx.Redirect(issue.Link())
		return
	}

	time.Sleep(1 * time.Second)

	ctx.Flash.Success(ctx.Tr("repo.pulls.update_branch_success"))
	ctx.Redirect(issue.Link())
}

// MergePullRequest response for merging pull request
func MergePullRequest(ctx *context.Context) {
	form := web.GetForm(ctx).(*forms.MergePullRequestForm)
	issue := checkPullInfo(ctx)
	if ctx.Written() {
		return
	}

	pr := issue.PullRequest
	pr.Issue = issue
	pr.Issue.Repo = ctx.Repo.Repository
	manuallMerge := repo_model.MergeStyle(form.Do) == repo_model.MergeStyleManuallyMerged
	forceMerge := form.ForceMerge != nil && *form.ForceMerge

	// start with merging by checking
	if err := pull_service.CheckPullMergable(ctx, ctx.Doer, &ctx.Repo.Permission, pr, manuallMerge, forceMerge); err != nil {
		if errors.Is(err, pull_service.ErrIsClosed) {
			if issue.IsPull {
				ctx.Flash.Error(ctx.Tr("repo.pulls.is_closed"))
				ctx.Redirect(issue.Link())
			} else {
				ctx.Flash.Error(ctx.Tr("repo.issues.closed_title"))
				ctx.Redirect(issue.Link())
			}
		} else if errors.Is(err, pull_service.ErrUserNotAllowedToMerge) {
			ctx.Flash.Error(ctx.Tr("repo.pulls.update_not_allowed"))
			ctx.Redirect(issue.Link())
		} else if errors.Is(err, pull_service.ErrHasMerged) {
			ctx.Flash.Error(ctx.Tr("repo.pulls.has_merged"))
			ctx.Redirect(issue.Link())
		} else if errors.Is(err, pull_service.ErrIsWorkInProgress) {
			ctx.Flash.Error(ctx.Tr("repo.pulls.no_merge_wip"))
			ctx.Redirect(issue.Link())
		} else if errors.Is(err, pull_service.ErrNotMergableState) {
			ctx.Flash.Error(ctx.Tr("repo.pulls.no_merge_not_ready"))
			ctx.Redirect(issue.Link())
		} else if models.IsErrDisallowedToMerge(err) {
			ctx.Flash.Error(ctx.Tr("repo.pulls.no_merge_not_ready"))
			ctx.Redirect(issue.Link())
		} else if asymkey_service.IsErrWontSign(err) {
			ctx.Flash.Error(err.Error()) // has not translation ...
			ctx.Redirect(issue.Link())
		} else if errors.Is(err, pull_service.ErrDependenciesLeft) {
			ctx.Flash.Error(ctx.Tr("repo.issues.dependency.pr_close_blocked"))
			ctx.Redirect(issue.Link())
		} else {
			ctx.ServerError("WebCheck", err)
		}
		return
	}

	// handle manually-merged mark
	if manuallMerge {
		if err := pull_service.MergedManually(pr, ctx.Doer, ctx.Repo.GitRepo, form.MergeCommitID); err != nil {
			if models.IsErrInvalidMergeStyle(err) {
				ctx.Flash.Error(ctx.Tr("repo.pulls.invalid_merge_option"))
				ctx.Redirect(issue.Link())
			} else if strings.Contains(err.Error(), "Wrong commit ID") {
				ctx.Flash.Error(ctx.Tr("repo.pulls.wrong_commit_id"))
				ctx.Redirect(issue.Link())
			} else {
				ctx.ServerError("MergedManually", err)
			}
			return
		}

		ctx.Redirect(issue.Link())
		return
	}

	// set defaults to propagate needed fields
	if err := form.SetDefaults(ctx, pr); err != nil {
		ctx.ServerError("SetDefaults", fmt.Errorf("SetDefaults: %v", err))
		return
	}

	if err := pull_service.Merge(pr, ctx.Doer, ctx.Repo.GitRepo, repo_model.MergeStyle(form.Do), form.HeadCommitID, form.MergeTitleField); err != nil {
		if models.IsErrInvalidMergeStyle(err) {
			ctx.Flash.Error(ctx.Tr("repo.pulls.invalid_merge_option"))
			ctx.Redirect(issue.Link())
		} else if models.IsErrMergeConflicts(err) {
			conflictError := err.(models.ErrMergeConflicts)
			flashError, err := ctx.RenderToString(tplAlertDetails, map[string]interface{}{
				"Message": ctx.Tr("repo.editor.merge_conflict"),
				"Summary": ctx.Tr("repo.editor.merge_conflict_summary"),
				"Details": utils.SanitizeFlashErrorString(conflictError.StdErr) + "<br>" + utils.SanitizeFlashErrorString(conflictError.StdOut),
			})
			if err != nil {
				ctx.ServerError("MergePullRequest.HTMLString", err)
				return
			}
			ctx.Flash.Error(flashError)
			ctx.Redirect(issue.Link())
		} else if models.IsErrRebaseConflicts(err) {
			conflictError := err.(models.ErrRebaseConflicts)
			flashError, err := ctx.RenderToString(tplAlertDetails, map[string]interface{}{
				"Message": ctx.Tr("repo.pulls.rebase_conflict", utils.SanitizeFlashErrorString(conflictError.CommitSHA)),
				"Summary": ctx.Tr("repo.pulls.rebase_conflict_summary"),
				"Details": utils.SanitizeFlashErrorString(conflictError.StdErr) + "<br>" + utils.SanitizeFlashErrorString(conflictError.StdOut),
			})
			if err != nil {
				ctx.ServerError("MergePullRequest.HTMLString", err)
				return
			}
			ctx.Flash.Error(flashError)
			ctx.Redirect(issue.Link())
		} else if models.IsErrMergeUnrelatedHistories(err) {
			log.Debug("MergeUnrelatedHistories error: %v", err)
			ctx.Flash.Error(ctx.Tr("repo.pulls.unrelated_histories"))
			ctx.Redirect(issue.Link())
		} else if git.IsErrPushOutOfDate(err) {
			log.Debug("MergePushOutOfDate error: %v", err)
			ctx.Flash.Error(ctx.Tr("repo.pulls.merge_out_of_date"))
			ctx.Redirect(issue.Link())
		} else if models.IsErrSHADoesNotMatch(err) {
			log.Debug("MergeHeadOutOfDate error: %v", err)
			ctx.Flash.Error(ctx.Tr("repo.pulls.head_out_of_date"))
			ctx.Redirect(issue.Link())
		} else if git.IsErrPushRejected(err) {
			log.Debug("MergePushRejected error: %v", err)
			pushrejErr := err.(*git.ErrPushRejected)
			message := pushrejErr.Message
			if len(message) == 0 {
				ctx.Flash.Error(ctx.Tr("repo.pulls.push_rejected_no_message"))
			} else {
				flashError, err := ctx.RenderToString(tplAlertDetails, map[string]interface{}{
					"Message": ctx.Tr("repo.pulls.push_rejected"),
					"Summary": ctx.Tr("repo.pulls.push_rejected_summary"),
					"Details": utils.SanitizeFlashErrorString(pushrejErr.Message),
				})
				if err != nil {
					ctx.ServerError("MergePullRequest.HTMLString", err)
					return
				}
				ctx.Flash.Error(flashError)
			}
			ctx.Redirect(issue.Link())
		} else {
			ctx.ServerError("Merge", err)
		}
		return
	}
	log.Trace("Pull request merged: %d", pr.ID)

	if err := stopTimerIfAvailable(ctx.Doer, issue); err != nil {
		ctx.ServerError("CreateOrStopIssueStopwatch", err)
		return
	}

	log.Trace("Pull request merged: %d", pr.ID)

	if form.DeleteBranchAfterMerge {
		// Don't cleanup when other pr use this branch as head branch
		exist, err := models.HasUnmergedPullRequestsByHeadInfo(ctx, pr.HeadRepoID, pr.HeadBranch)
		if err != nil {
			ctx.ServerError("HasUnmergedPullRequestsByHeadInfo", err)
			return
		}
		if exist {
			ctx.Redirect(issue.Link())
			return
		}

		var headRepo *git.Repository
		if ctx.Repo != nil && ctx.Repo.Repository != nil && pr.HeadRepoID == ctx.Repo.Repository.ID && ctx.Repo.GitRepo != nil {
			headRepo = ctx.Repo.GitRepo
		} else {
			headRepo, err = git.OpenRepository(ctx, pr.HeadRepo.RepoPath())
			if err != nil {
				ctx.ServerError(fmt.Sprintf("OpenRepository[%s]", pr.HeadRepo.RepoPath()), err)
				return
			}
			defer headRepo.Close()
		}
		deleteBranch(ctx, pr, headRepo)
	}

	ctx.Redirect(issue.Link())
}

func stopTimerIfAvailable(user *user_model.User, issue *models.Issue) error {
	if models.StopwatchExists(user.ID, issue.ID) {
		if err := models.CreateOrStopIssueStopwatch(user, issue); err != nil {
			return err
		}
	}

	return nil
}

// CompareAndPullRequestPost response for creating pull request
func CompareAndPullRequestPost(ctx *context.Context) {
	form := web.GetForm(ctx).(*forms.CreateIssueForm)
	ctx.Data["Title"] = ctx.Tr("repo.pulls.compare_changes")
	ctx.Data["PageIsComparePull"] = true
	ctx.Data["IsDiffCompare"] = true
	ctx.Data["IsRepoToolbarCommits"] = true
	ctx.Data["RequireTribute"] = true
	ctx.Data["PullRequestWorkInProgressPrefixes"] = setting.Repository.PullRequest.WorkInProgressPrefixes
	ctx.Data["IsAttachmentEnabled"] = setting.Attachment.Enabled
	upload.AddUploadContext(ctx, "comment")
	ctx.Data["HasIssuesOrPullsWritePermission"] = ctx.Repo.CanWrite(unit.TypePullRequests)

	var (
		repo        = ctx.Repo.Repository
		attachments []string
	)

	ci := ParseCompareInfo(ctx)
	defer func() {
		if ci != nil && ci.HeadGitRepo != nil {
			ci.HeadGitRepo.Close()
		}
	}()
	if ctx.Written() {
		return
	}

	labelIDs, assigneeIDs, milestoneID, _ := ValidateRepoMetas(ctx, *form, true)
	if ctx.Written() {
		return
	}

	if setting.Attachment.Enabled {
		attachments = form.Files
	}

	if ctx.HasError() {
		middleware.AssignForm(form, ctx.Data)

		// This stage is already stop creating new pull request, so it does not matter if it has
		// something to compare or not.
		PrepareCompareDiff(ctx, ci,
			gitdiff.GetWhitespaceFlag(ctx.Data["WhitespaceBehavior"].(string)))
		if ctx.Written() {
			return
		}

		if len(form.Title) > 255 {
			var trailer string
			form.Title, trailer = util.SplitStringAtByteN(form.Title, 255)

			form.Content = trailer + "\n\n" + form.Content
		}
		middleware.AssignForm(form, ctx.Data)

		ctx.HTML(http.StatusOK, tplCompareDiff)
		return
	}

	if util.IsEmptyString(form.Title) {
		PrepareCompareDiff(ctx, ci,
			gitdiff.GetWhitespaceFlag(ctx.Data["WhitespaceBehavior"].(string)))
		if ctx.Written() {
			return
		}

		ctx.RenderWithErr(ctx.Tr("repo.issues.new.title_empty"), tplCompareDiff, form)
		return
	}

	pullIssue := &models.Issue{
		RepoID:      repo.ID,
		Repo:        repo,
		Title:       form.Title,
		PosterID:    ctx.Doer.ID,
		Poster:      ctx.Doer,
		MilestoneID: milestoneID,
		IsPull:      true,
		Content:     form.Content,
	}
	pullRequest := &models.PullRequest{
		HeadRepoID:          ci.HeadRepo.ID,
		BaseRepoID:          repo.ID,
		HeadBranch:          ci.HeadBranch,
		BaseBranch:          ci.BaseBranch,
		HeadRepo:            ci.HeadRepo,
		BaseRepo:            repo,
		MergeBase:           ci.CompareInfo.MergeBase,
		Type:                models.PullRequestGitea,
		AllowMaintainerEdit: form.AllowMaintainerEdit,
	}
	// FIXME: check error in the case two people send pull request at almost same time, give nice error prompt
	// instead of 500.

	if err := pull_service.NewPullRequest(ctx, repo, pullIssue, labelIDs, attachments, pullRequest, assigneeIDs); err != nil {
		if models.IsErrUserDoesNotHaveAccessToRepo(err) {
			ctx.Error(http.StatusBadRequest, "UserDoesNotHaveAccessToRepo", err.Error())
			return
		} else if git.IsErrPushRejected(err) {
			pushrejErr := err.(*git.ErrPushRejected)
			message := pushrejErr.Message
			if len(message) == 0 {
				ctx.Flash.Error(ctx.Tr("repo.pulls.push_rejected_no_message"))
			} else {
				flashError, err := ctx.RenderToString(tplAlertDetails, map[string]interface{}{
					"Message": ctx.Tr("repo.pulls.push_rejected"),
					"Summary": ctx.Tr("repo.pulls.push_rejected_summary"),
					"Details": utils.SanitizeFlashErrorString(pushrejErr.Message),
				})
				if err != nil {
					ctx.ServerError("CompareAndPullRequest.HTMLString", err)
					return
				}
				ctx.Flash.Error(flashError)
			}
			ctx.Redirect(pullIssue.Link())
			return
		}
		ctx.ServerError("NewPullRequest", err)
		return
	}

	log.Trace("Pull request created: %d/%d", repo.ID, pullIssue.ID)
	ctx.Redirect(pullIssue.Link())
}

// CleanUpPullRequest responses for delete merged branch when PR has been merged
func CleanUpPullRequest(ctx *context.Context) {
	issue := checkPullInfo(ctx)
	if ctx.Written() {
		return
	}

	pr := issue.PullRequest

	// Don't cleanup unmerged and unclosed PRs
	if !pr.HasMerged && !issue.IsClosed {
		ctx.NotFound("CleanUpPullRequest", nil)
		return
	}

	// Don't cleanup when there are other PR's that use this branch as head branch.
	exist, err := models.HasUnmergedPullRequestsByHeadInfo(ctx, pr.HeadRepoID, pr.HeadBranch)
	if err != nil {
		ctx.ServerError("HasUnmergedPullRequestsByHeadInfo", err)
		return
	}
	if exist {
		ctx.NotFound("CleanUpPullRequest", nil)
		return
	}

	if err := pr.LoadHeadRepoCtx(ctx); err != nil {
		ctx.ServerError("LoadHeadRepo", err)
		return
	} else if pr.HeadRepo == nil {
		// Forked repository has already been deleted
		ctx.NotFound("CleanUpPullRequest", nil)
		return
	} else if err = pr.LoadBaseRepoCtx(ctx); err != nil {
		ctx.ServerError("LoadBaseRepo", err)
		return
	} else if err = pr.HeadRepo.GetOwner(ctx); err != nil {
		ctx.ServerError("HeadRepo.GetOwner", err)
		return
	}

	perm, err := models.GetUserRepoPermission(ctx, pr.HeadRepo, ctx.Doer)
	if err != nil {
		ctx.ServerError("GetUserRepoPermission", err)
		return
	}
	if !perm.CanWrite(unit.TypeCode) {
		ctx.NotFound("CleanUpPullRequest", nil)
		return
	}

	fullBranchName := pr.HeadRepo.Owner.Name + "/" + pr.HeadBranch

	var gitBaseRepo *git.Repository

	// Assume that the base repo is the current context (almost certainly)
	if ctx.Repo != nil && ctx.Repo.Repository != nil && ctx.Repo.Repository.ID == pr.BaseRepoID && ctx.Repo.GitRepo != nil {
		gitBaseRepo = ctx.Repo.GitRepo
	} else {
		// If not just open it
		gitBaseRepo, err = git.OpenRepository(ctx, pr.BaseRepo.RepoPath())
		if err != nil {
			ctx.ServerError(fmt.Sprintf("OpenRepository[%s]", pr.BaseRepo.RepoPath()), err)
			return
		}
		defer gitBaseRepo.Close()
	}

	// Now assume that the head repo is the same as the base repo (reasonable chance)
	gitRepo := gitBaseRepo
	// But if not: is it the same as the context?
	if pr.BaseRepoID != pr.HeadRepoID && ctx.Repo != nil && ctx.Repo.Repository != nil && ctx.Repo.Repository.ID == pr.HeadRepoID && ctx.Repo.GitRepo != nil {
		gitRepo = ctx.Repo.GitRepo
	} else if pr.BaseRepoID != pr.HeadRepoID {
		// Otherwise just load it up
		gitRepo, err = git.OpenRepository(ctx, pr.HeadRepo.RepoPath())
		if err != nil {
			ctx.ServerError(fmt.Sprintf("OpenRepository[%s]", pr.HeadRepo.RepoPath()), err)
			return
		}
		defer gitRepo.Close()
	}

	defer func() {
		ctx.JSON(http.StatusOK, map[string]interface{}{
			"redirect": issue.Link(),
		})
	}()

	// Check if branch has no new commits
	headCommitID, err := gitBaseRepo.GetRefCommitID(pr.GetGitRefName())
	if err != nil {
		log.Error("GetRefCommitID: %v", err)
		ctx.Flash.Error(ctx.Tr("repo.branch.deletion_failed", fullBranchName))
		return
	}
	branchCommitID, err := gitRepo.GetBranchCommitID(pr.HeadBranch)
	if err != nil {
		log.Error("GetBranchCommitID: %v", err)
		ctx.Flash.Error(ctx.Tr("repo.branch.deletion_failed", fullBranchName))
		return
	}
	if headCommitID != branchCommitID {
		ctx.Flash.Error(ctx.Tr("repo.branch.delete_branch_has_new_commits", fullBranchName))
		return
	}

	deleteBranch(ctx, pr, gitRepo)
}

func deleteBranch(ctx *context.Context, pr *models.PullRequest, gitRepo *git.Repository) {
	fullBranchName := pr.HeadRepo.Owner.Name + "/" + pr.HeadBranch
	if err := repo_service.DeleteBranch(ctx.Doer, pr.HeadRepo, gitRepo, pr.HeadBranch); err != nil {
		switch {
		case git.IsErrBranchNotExist(err):
			ctx.Flash.Error(ctx.Tr("repo.branch.deletion_failed", fullBranchName))
		case errors.Is(err, repo_service.ErrBranchIsDefault):
			ctx.Flash.Error(ctx.Tr("repo.branch.deletion_failed", fullBranchName))
		case errors.Is(err, repo_service.ErrBranchIsProtected):
			ctx.Flash.Error(ctx.Tr("repo.branch.deletion_failed", fullBranchName))
		default:
			log.Error("DeleteBranch: %v", err)
			ctx.Flash.Error(ctx.Tr("repo.branch.deletion_failed", fullBranchName))
		}
		return
	}

	if err := models.AddDeletePRBranchComment(ctx, ctx.Doer, pr.BaseRepo, pr.IssueID, pr.HeadBranch); err != nil {
		// Do not fail here as branch has already been deleted
		log.Error("DeleteBranch: %v", err)
	}

	ctx.Flash.Success(ctx.Tr("repo.branch.deletion_success", fullBranchName))
}

// DownloadPullDiff render a pull's raw diff
func DownloadPullDiff(ctx *context.Context) {
	DownloadPullDiffOrPatch(ctx, false)
}

// DownloadPullPatch render a pull's raw patch
func DownloadPullPatch(ctx *context.Context) {
	DownloadPullDiffOrPatch(ctx, true)
}

// DownloadPullDiffOrPatch render a pull's raw diff or patch
func DownloadPullDiffOrPatch(ctx *context.Context, patch bool) {
	pr, err := models.GetPullRequestByIndex(ctx.Repo.Repository.ID, ctx.ParamsInt64(":index"))
	if err != nil {
		if models.IsErrPullRequestNotExist(err) {
			ctx.NotFound("GetPullRequestByIndex", err)
		} else {
			ctx.ServerError("GetPullRequestByIndex", err)
		}
		return
	}

	binary := ctx.FormBool("binary")

	if err := pull_service.DownloadDiffOrPatch(ctx, pr, ctx, patch, binary); err != nil {
		ctx.ServerError("DownloadDiffOrPatch", err)
		return
	}
}

// UpdatePullRequestTarget change pull request's target branch
func UpdatePullRequestTarget(ctx *context.Context) {
	issue := GetActionIssue(ctx)
	pr := issue.PullRequest
	if ctx.Written() {
		return
	}
	if !issue.IsPull {
		ctx.Error(http.StatusNotFound)
		return
	}

	if !ctx.IsSigned || (!issue.IsPoster(ctx.Doer.ID) && !ctx.Repo.CanWriteIssuesOrPulls(issue.IsPull)) {
		ctx.Error(http.StatusForbidden)
		return
	}

	targetBranch := ctx.FormTrim("target_branch")
	if len(targetBranch) == 0 {
		ctx.Error(http.StatusNoContent)
		return
	}

	if err := pull_service.ChangeTargetBranch(ctx, pr, ctx.Doer, targetBranch); err != nil {
		if models.IsErrPullRequestAlreadyExists(err) {
			err := err.(models.ErrPullRequestAlreadyExists)

			RepoRelPath := ctx.Repo.Owner.Name + "/" + ctx.Repo.Repository.Name
			errorMessage := ctx.Tr("repo.pulls.has_pull_request", html.EscapeString(ctx.Repo.RepoLink+"/pulls/"+strconv.FormatInt(err.IssueID, 10)), html.EscapeString(RepoRelPath), err.IssueID) // FIXME: Creates url insidde locale string

			ctx.Flash.Error(errorMessage)
			ctx.JSON(http.StatusConflict, map[string]interface{}{
				"error":      err.Error(),
				"user_error": errorMessage,
			})
		} else if models.IsErrIssueIsClosed(err) {
			errorMessage := ctx.Tr("repo.pulls.is_closed")

			ctx.Flash.Error(errorMessage)
			ctx.JSON(http.StatusConflict, map[string]interface{}{
				"error":      err.Error(),
				"user_error": errorMessage,
			})
		} else if models.IsErrPullRequestHasMerged(err) {
			errorMessage := ctx.Tr("repo.pulls.has_merged")

			ctx.Flash.Error(errorMessage)
			ctx.JSON(http.StatusConflict, map[string]interface{}{
				"error":      err.Error(),
				"user_error": errorMessage,
			})
		} else if models.IsErrBranchesEqual(err) {
			errorMessage := ctx.Tr("repo.pulls.nothing_to_compare")

			ctx.Flash.Error(errorMessage)
			ctx.JSON(http.StatusBadRequest, map[string]interface{}{
				"error":      err.Error(),
				"user_error": errorMessage,
			})
		} else {
			ctx.ServerError("UpdatePullRequestTarget", err)
		}
		return
	}
	notification.NotifyPullRequestChangeTargetBranch(ctx.Doer, pr, targetBranch)

	ctx.JSON(http.StatusOK, map[string]interface{}{
		"base_branch": pr.BaseBranch,
	})
}

// SetAllowEdits allow edits from maintainers to PRs
func SetAllowEdits(ctx *context.Context) {
	form := web.GetForm(ctx).(*forms.UpdateAllowEditsForm)

	pr, err := models.GetPullRequestByIndex(ctx.Repo.Repository.ID, ctx.ParamsInt64(":index"))
	if err != nil {
		if models.IsErrPullRequestNotExist(err) {
			ctx.NotFound("GetPullRequestByIndex", err)
		} else {
			ctx.ServerError("GetPullRequestByIndex", err)
		}
		return
	}

	if err := pull_service.SetAllowEdits(ctx, ctx.Doer, pr, form.AllowMaintainerEdit); err != nil {
		if errors.Is(pull_service.ErrUserHasNoPermissionForAction, err) {
			ctx.Error(http.StatusForbidden)
			return
		}
		ctx.ServerError("SetAllowEdits", err)
		return
	}

	ctx.JSON(http.StatusOK, map[string]interface{}{
		"allow_maintainer_edit": pr.AllowMaintainerEdit,
	})
}
