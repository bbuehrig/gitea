// Copyright 2020 The Gitea Authors. All rights reserved.
// Use of this source code is governed by a MIT-style
// license that can be found in the LICENSE file.

package doctor

import (
	"context"

	"code.gitea.io/gitea/models"
	"code.gitea.io/gitea/models/db"
	issues_model "code.gitea.io/gitea/models/issues"
	"code.gitea.io/gitea/models/migrations"
	repo_model "code.gitea.io/gitea/models/repo"
	"code.gitea.io/gitea/modules/log"
	"code.gitea.io/gitea/modules/setting"
)

type consistencyCheck struct {
	Name         string
	Counter      func() (int64, error)
	Fixer        func() (int64, error)
	FixedMessage string
}

func (c *consistencyCheck) Run(ctx context.Context, logger log.Logger, autofix bool) error {
	count, err := c.Counter()
	if err != nil {
		logger.Critical("Error: %v whilst counting %s", err, c.Name)
		return err
	}
	if count > 0 {
		if autofix {
			var fixed int64
			if fixed, err = c.Fixer(); err != nil {
				logger.Critical("Error: %v whilst fixing %s", err, c.Name)
				return err
			}

			prompt := "Deleted"
			if c.FixedMessage != "" {
				prompt = c.FixedMessage
			}

			if fixed < 0 {
				logger.Info(prompt+" %d %s", count, c.Name)
			} else {
				logger.Info(prompt+" %d/%d %s", fixed, count, c.Name)
			}
		} else {
			logger.Warn("Found %d %s", count, c.Name)
		}
	}
	return nil
}

func asFixer(fn func() error) func() (int64, error) {
	return func() (int64, error) {
		err := fn()
		return -1, err
	}
}

func genericOrphanCheck(name, subject, refobject, joincond string) consistencyCheck {
	return consistencyCheck{
		Name: name,
		Counter: func() (int64, error) {
			return db.CountOrphanedObjects(subject, refobject, joincond)
		},
		Fixer: func() (int64, error) {
			err := db.DeleteOrphanedObjects(subject, refobject, joincond)
			return -1, err
		},
	}
}

func checkDBConsistency(ctx context.Context, logger log.Logger, autofix bool) error {
	// make sure DB version is uptodate
	if err := db.InitEngineWithMigration(ctx, migrations.EnsureUpToDate); err != nil {
		logger.Critical("Model version on the database does not match the current Gitea version. Model consistency will not be checked until the database is upgraded")
		return err
	}

	consistencyChecks := []consistencyCheck{
		{
			// find labels without existing repo or org
			Name:    "Orphaned Labels without existing repository or organisation",
			Counter: issues_model.CountOrphanedLabels,
			Fixer:   asFixer(issues_model.DeleteOrphanedLabels),
		},
		{
			// find IssueLabels without existing label
			Name:    "Orphaned Issue Labels without existing label",
			Counter: issues_model.CountOrphanedIssueLabels,
			Fixer:   asFixer(issues_model.DeleteOrphanedIssueLabels),
		},
		{
			// find issues without existing repository
			Name:    "Orphaned Issues without existing repository",
			Counter: issues_model.CountOrphanedIssues,
			Fixer:   asFixer(issues_model.DeleteOrphanedIssues),
		},
		// find releases without existing repository
		genericOrphanCheck("Orphaned Releases without existing repository",
			"release", "repository", "release.repo_id=repository.id"),
		// find pulls without existing issues
		genericOrphanCheck("Orphaned PullRequests without existing issue",
			"pull_request", "issue", "pull_request.issue_id=issue.id"),
		// find pull requests without base repository
		genericOrphanCheck("Pull request entries without existing base repository",
			"pull_request", "repository", "pull_request.base_repo_id=repository.id"),
		// find tracked times without existing issues/pulls
		genericOrphanCheck("Orphaned TrackedTimes without existing issue",
			"tracked_time", "issue", "tracked_time.issue_id=issue.id"),
		// find attachments without existing issues or releases
		{
			Name:    "Orphaned Attachments without existing issues or releases",
			Counter: repo_model.CountOrphanedAttachments,
			Fixer:   asFixer(repo_model.DeleteOrphanedAttachments),
		},
		// find null archived repositories
		{
			Name:         "Repositories with is_archived IS NULL",
			Counter:      models.CountNullArchivedRepository,
			Fixer:        models.FixNullArchivedRepository,
			FixedMessage: "Fixed",
		},
		// find label comments with empty labels
		{
			Name:         "Label comments with empty labels",
			Counter:      issues_model.CountCommentTypeLabelWithEmptyLabel,
			Fixer:        issues_model.FixCommentTypeLabelWithEmptyLabel,
			FixedMessage: "Fixed",
		},
		// find label comments with labels from outside the repository
		{
			Name:         "Label comments with labels from outside the repository",
			Counter:      issues_model.CountCommentTypeLabelWithOutsideLabels,
			Fixer:        issues_model.FixCommentTypeLabelWithOutsideLabels,
			FixedMessage: "Removed",
		},
		// find issue_label with labels from outside the repository
		{
			Name:         "IssueLabels with Labels from outside the repository",
			Counter:      issues_model.CountIssueLabelWithOutsideLabels,
			Fixer:        issues_model.FixIssueLabelWithOutsideLabels,
			FixedMessage: "Removed",
		},
		{
			Name:         "Action with created_unix set as an empty string",
			Counter:      models.CountActionCreatedUnixString,
			Fixer:        models.FixActionCreatedUnixString,
			FixedMessage: "Set to zero",
		},
	}

	// TODO: function to recalc all counters

	if setting.Database.UsePostgreSQL {
		consistencyChecks = append(consistencyChecks, consistencyCheck{
			Name:         "Sequence values",
			Counter:      db.CountBadSequences,
			Fixer:        asFixer(db.FixBadSequences),
			FixedMessage: "Updated",
		})
	}

	consistencyChecks = append(consistencyChecks,
		// find protected branches without existing repository
		genericOrphanCheck("Protected Branches without existing repository",
			"protected_branch", "repository", "protected_branch.repo_id=repository.id"),
		// find deleted branches without existing repository
		genericOrphanCheck("Deleted Branches without existing repository",
			"deleted_branch", "repository", "deleted_branch.repo_id=repository.id"),
		// find LFS locks without existing repository
		genericOrphanCheck("LFS locks without existing repository",
			"lfs_lock", "repository", "lfs_lock.repo_id=repository.id"),
		// find collaborations without users
		genericOrphanCheck("Collaborations without existing user",
			"collaboration", "user", "collaboration.user_id=`user`.id"),
		// find collaborations without repository
		genericOrphanCheck("Collaborations without existing repository",
			"collaboration", "repository", "collaboration.repo_id=repository.id"),
		// find access without users
		genericOrphanCheck("Access entries without existing user",
			"access", "user", "access.user_id=`user`.id"),
		// find access without repository
		genericOrphanCheck("Access entries without existing repository",
			"access", "repository", "access.repo_id=repository.id"),
		// find action without repository
		genericOrphanCheck("Action entries without existing repository",
			"action", "repository", "action.repo_id=repository.id"),
		// find OAuth2Grant without existing user
		genericOrphanCheck("Orphaned OAuth2Grant without existing User",
			"oauth2_grant", "user", "oauth2_grant.user_id=`user`.id"),
		// find OAuth2Application without existing user
		genericOrphanCheck("Orphaned OAuth2Application without existing User",
			"oauth2_application", "user", "oauth2_application.uid=`user`.id"),
		// find OAuth2AuthorizationCode without existing OAuth2Grant
		genericOrphanCheck("Orphaned OAuth2AuthorizationCode without existing OAuth2Grant",
			"oauth2_authorization_code", "oauth2_grant", "oauth2_authorization_code.grant_id=oauth2_grant.id"),
	)

	for _, c := range consistencyChecks {
		if err := c.Run(ctx, logger, autofix); err != nil {
			return err
		}
	}

	return nil
}

func init() {
	Register(&Check{
		Title:     "Check consistency of database",
		Name:      "check-db-consistency",
		IsDefault: false,
		Run:       checkDBConsistency,
		Priority:  3,
	})
}
