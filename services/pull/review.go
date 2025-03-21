// Copyright 2019 The Gitea Authors.
// All rights reserved.
// Use of this source code is governed by a MIT-style
// license that can be found in the LICENSE file.

package pull

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strings"

	"code.gitea.io/gitea/models/db"
	issues_model "code.gitea.io/gitea/models/issues"
	repo_model "code.gitea.io/gitea/models/repo"
	user_model "code.gitea.io/gitea/models/user"
	"code.gitea.io/gitea/modules/git"
	"code.gitea.io/gitea/modules/log"
	"code.gitea.io/gitea/modules/notification"
	"code.gitea.io/gitea/modules/setting"
)

// CreateCodeComment creates a comment on the code line
func CreateCodeComment(ctx context.Context, doer *user_model.User, gitRepo *git.Repository, issue *issues_model.Issue, line int64, content, treePath string, isReview bool, replyReviewID int64, latestCommitID string) (*issues_model.Comment, error) {
	var (
		existsReview bool
		err          error
	)

	// CreateCodeComment() is used for:
	// - Single comments
	// - Comments that are part of a review
	// - Comments that reply to an existing review

	if !isReview && replyReviewID != 0 {
		// It's not part of a review; maybe a reply to a review comment or a single comment.
		// Check if there are reviews for that line already; if there are, this is a reply
		if existsReview, err = issues_model.ReviewExists(issue, treePath, line); err != nil {
			return nil, err
		}
	}

	// Comments that are replies don't require a review header to show up in the issue view
	if !isReview && existsReview {
		if err = issue.LoadRepo(ctx); err != nil {
			return nil, err
		}

		comment, err := createCodeComment(ctx,
			doer,
			issue.Repo,
			issue,
			content,
			treePath,
			line,
			replyReviewID,
		)
		if err != nil {
			return nil, err
		}

		mentions, err := issues_model.FindAndUpdateIssueMentions(ctx, issue, doer, comment.Content)
		if err != nil {
			return nil, err
		}

		notification.NotifyCreateIssueComment(doer, issue.Repo, issue, comment, mentions)

		return comment, nil
	}

	review, err := issues_model.GetCurrentReview(ctx, doer, issue)
	if err != nil {
		if !issues_model.IsErrReviewNotExist(err) {
			return nil, err
		}

		if review, err = issues_model.CreateReview(ctx, issues_model.CreateReviewOptions{
			Type:     issues_model.ReviewTypePending,
			Reviewer: doer,
			Issue:    issue,
			Official: false,
			CommitID: latestCommitID,
		}); err != nil {
			return nil, err
		}
	}

	comment, err := createCodeComment(ctx,
		doer,
		issue.Repo,
		issue,
		content,
		treePath,
		line,
		review.ID,
	)
	if err != nil {
		return nil, err
	}

	if !isReview && !existsReview {
		// Submit the review we've just created so the comment shows up in the issue view
		if _, _, err = SubmitReview(ctx, doer, gitRepo, issue, issues_model.ReviewTypeComment, "", latestCommitID, nil); err != nil {
			return nil, err
		}
	}

	// NOTICE: if it's a pending review the notifications will not be fired until user submit review.

	return comment, nil
}

var notEnoughLines = regexp.MustCompile(`exit status 128 - fatal: file .* has only \d+ lines?`)

// createCodeComment creates a plain code comment at the specified line / path
func createCodeComment(ctx context.Context, doer *user_model.User, repo *repo_model.Repository, issue *issues_model.Issue, content, treePath string, line, reviewID int64) (*issues_model.Comment, error) {
	var commitID, patch string
	if err := issue.LoadPullRequest(); err != nil {
		return nil, fmt.Errorf("GetPullRequestByIssueID: %v", err)
	}
	pr := issue.PullRequest
	if err := pr.LoadBaseRepoCtx(ctx); err != nil {
		return nil, fmt.Errorf("LoadHeadRepo: %v", err)
	}
	gitRepo, closer, err := git.RepositoryFromContextOrOpen(ctx, pr.BaseRepo.RepoPath())
	if err != nil {
		return nil, fmt.Errorf("RepositoryFromContextOrOpen: %v", err)
	}
	defer closer.Close()

	invalidated := false
	head := pr.GetGitRefName()
	if line > 0 {
		if reviewID != 0 {
			first, err := issues_model.FindComments(ctx, &issues_model.FindCommentsOptions{
				ReviewID: reviewID,
				Line:     line,
				TreePath: treePath,
				Type:     issues_model.CommentTypeCode,
				ListOptions: db.ListOptions{
					PageSize: 1,
					Page:     1,
				},
			})
			if err == nil && len(first) > 0 {
				commitID = first[0].CommitSHA
				invalidated = first[0].Invalidated
				patch = first[0].Patch
			} else if err != nil && !issues_model.IsErrCommentNotExist(err) {
				return nil, fmt.Errorf("Find first comment for %d line %d path %s. Error: %v", reviewID, line, treePath, err)
			} else {
				review, err := issues_model.GetReviewByID(ctx, reviewID)
				if err == nil && len(review.CommitID) > 0 {
					head = review.CommitID
				} else if err != nil && !issues_model.IsErrReviewNotExist(err) {
					return nil, fmt.Errorf("GetReviewByID %d. Error: %v", reviewID, err)
				}
			}
		}

		if len(commitID) == 0 {
			// FIXME validate treePath
			// Get latest commit referencing the commented line
			// No need for get commit for base branch changes
			commit, err := gitRepo.LineBlame(head, gitRepo.Path, treePath, uint(line))
			if err == nil {
				commitID = commit.ID.String()
			} else if !(strings.Contains(err.Error(), "exit status 128 - fatal: no such path") || notEnoughLines.MatchString(err.Error())) {
				return nil, fmt.Errorf("LineBlame[%s, %s, %s, %d]: %v", pr.GetGitRefName(), gitRepo.Path, treePath, line, err)
			}
		}
	}

	// Only fetch diff if comment is review comment
	if len(patch) == 0 && reviewID != 0 {
		headCommitID, err := gitRepo.GetRefCommitID(pr.GetGitRefName())
		if err != nil {
			return nil, fmt.Errorf("GetRefCommitID[%s]: %v", pr.GetGitRefName(), err)
		}
		if len(commitID) == 0 {
			commitID = headCommitID
		}
		reader, writer := io.Pipe()
		defer func() {
			_ = reader.Close()
			_ = writer.Close()
		}()
		go func() {
			if err := git.GetRepoRawDiffForFile(gitRepo, pr.MergeBase, headCommitID, git.RawDiffNormal, treePath, writer); err != nil {
				_ = writer.CloseWithError(fmt.Errorf("GetRawDiffForLine[%s, %s, %s, %s]: %v", gitRepo.Path, pr.MergeBase, headCommitID, treePath, err))
				return
			}
			_ = writer.Close()
		}()

		patch, err = git.CutDiffAroundLine(reader, int64((&issues_model.Comment{Line: line}).UnsignedLine()), line < 0, setting.UI.CodeCommentLines)
		if err != nil {
			log.Error("Error whilst generating patch: %v", err)
			return nil, err
		}
	}
	return issues_model.CreateComment(&issues_model.CreateCommentOptions{
		Type:        issues_model.CommentTypeCode,
		Doer:        doer,
		Repo:        repo,
		Issue:       issue,
		Content:     content,
		LineNum:     line,
		TreePath:    treePath,
		CommitSHA:   commitID,
		ReviewID:    reviewID,
		Patch:       patch,
		Invalidated: invalidated,
	})
}

// SubmitReview creates a review out of the existing pending review or creates a new one if no pending review exist
func SubmitReview(ctx context.Context, doer *user_model.User, gitRepo *git.Repository, issue *issues_model.Issue, reviewType issues_model.ReviewType, content, commitID string, attachmentUUIDs []string) (*issues_model.Review, *issues_model.Comment, error) {
	pr, err := issue.GetPullRequest()
	if err != nil {
		return nil, nil, err
	}

	var stale bool
	if reviewType != issues_model.ReviewTypeApprove && reviewType != issues_model.ReviewTypeReject {
		stale = false
	} else {
		headCommitID, err := gitRepo.GetRefCommitID(pr.GetGitRefName())
		if err != nil {
			return nil, nil, err
		}

		if headCommitID == commitID {
			stale = false
		} else {
			stale, err = checkIfPRContentChanged(ctx, pr, commitID, headCommitID)
			if err != nil {
				return nil, nil, err
			}
		}
	}

	review, comm, err := issues_model.SubmitReview(doer, issue, reviewType, content, commitID, stale, attachmentUUIDs)
	if err != nil {
		return nil, nil, err
	}

	mentions, err := issues_model.FindAndUpdateIssueMentions(ctx, issue, doer, comm.Content)
	if err != nil {
		return nil, nil, err
	}

	notification.NotifyPullRequestReview(pr, review, comm, mentions)

	for _, lines := range review.CodeComments {
		for _, comments := range lines {
			for _, codeComment := range comments {
				mentions, err := issues_model.FindAndUpdateIssueMentions(ctx, issue, doer, codeComment.Content)
				if err != nil {
					return nil, nil, err
				}
				notification.NotifyPullRequestCodeComment(pr, codeComment, mentions)
			}
		}
	}

	return review, comm, nil
}

// DismissReview dismissing stale review by repo admin
func DismissReview(ctx context.Context, reviewID int64, message string, doer *user_model.User, isDismiss bool) (comment *issues_model.Comment, err error) {
	review, err := issues_model.GetReviewByID(ctx, reviewID)
	if err != nil {
		return
	}

	if review.Type != issues_model.ReviewTypeApprove && review.Type != issues_model.ReviewTypeReject {
		return nil, fmt.Errorf("not need to dismiss this review because it's type is not Approve or change request")
	}

	if err = issues_model.DismissReview(review, isDismiss); err != nil {
		return
	}

	if !isDismiss {
		return nil, nil
	}

	// load data for notify
	if err = review.LoadAttributes(ctx); err != nil {
		return
	}
	if err = review.Issue.LoadPullRequest(); err != nil {
		return
	}
	if err = review.Issue.LoadAttributes(ctx); err != nil {
		return
	}

	comment, err = issues_model.CreateComment(&issues_model.CreateCommentOptions{
		Doer:     doer,
		Content:  message,
		Type:     issues_model.CommentTypeDismissReview,
		ReviewID: review.ID,
		Issue:    review.Issue,
		Repo:     review.Issue.Repo,
	})
	if err != nil {
		return
	}

	comment.Review = review
	comment.Poster = doer
	comment.Issue = review.Issue

	notification.NotifyPullRevieweDismiss(doer, review, comment)

	return
}
