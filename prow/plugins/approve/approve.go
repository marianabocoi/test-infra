/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package approve

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"k8s.io/test-infra/prow/github"
	"k8s.io/test-infra/prow/plugins"
	"k8s.io/test-infra/prow/plugins/approve/approvers"
)

const (
	pluginName = "approve"

	approveCommand  = "APPROVE"
	approvedLabel   = "approved"
	lgtmCommand     = "LGTM"
	cancelArgument  = "cancel"
	noIssueArgument = "no-issue"

	// deprecatedBotName is the name of the bot that previously handled approvals.
	// It can be removed once every PR approved by the old bot has been merged or unapproved.
	deprecatedBotName = "k8s-merge-robot"
)

var (
	associatedIssueRegex = regexp.MustCompile(`(?:kubernetes/[^/]+/issues/|#)(\d+)`)
	commandRegex         = regexp.MustCompile(`(?m)^/([^\s]+)[\t ]*([^\n\r]*)`)
	notificationRegex    = regexp.MustCompile(`(?is)^\[` + approvers.ApprovalNotificationName + `\] *?([^\n]*)(?:\n\n(.*))?`)
)

type githubClient interface {
	GetPullRequestChanges(org, repo string, number int) ([]github.PullRequestChange, error)
	GetIssueLabels(org, repo string, number int) ([]github.Label, error)
	ListIssueComments(org, repo string, number int) ([]github.IssueComment, error)
	ListReviews(org, repo string, number int) ([]github.Review, error)
	ListPullRequestComments(org, repo string, number int) ([]github.ReviewComment, error)
	DeleteComment(org, repo string, ID int) error
	CreateComment(org, repo string, number int, comment string) error
	BotName() (string, error)
	AddLabel(org, repo string, number int, label string) error
	RemoveLabel(org, repo string, number int, label string) error
	ListIssueEvents(org, repo string, num int) ([]github.ListedIssueEvent, error)
}

type state struct {
	org    string
	repo   string
	number int

	body      string
	author    string
	assignees []github.User
	htmlURL   string

	repoOptions *plugins.Approve
}

func init() {
	plugins.RegisterGenericCommentHandler(pluginName, handleGenericCommentEvent)
	plugins.RegisterPullRequestHandler(pluginName, handlePullRequestEvent)
}

func handleGenericCommentEvent(pc plugins.PluginClient, ce github.GenericCommentEvent) error {
	if ce.Action != github.GenericCommentActionCreated || !ce.IsPR || ce.IssueState == "closed" {
		return nil
	}
	botName, err := pc.GitHubClient.BotName()
	if err != nil {
		return err
	}

	if !approvalCommandMatcher(botName)(&comment{Body: ce.Body, Author: ce.User.Login}) {
		return nil
	}

	ro, err := pc.OwnersClient.LoadRepoOwners(ce.Repo.Owner.Login, ce.Repo.Name)
	if err != nil {
		return err
	}
	return handleGenericComment(pc.Logger, pc.GitHubClient, ro, pc.PluginConfig, &ce)
}

func handleGenericComment(log *logrus.Entry, ghc githubClient, repo approvers.RepoInterface, config *plugins.Configuration, ce *github.GenericCommentEvent) error {
	return handle(
		log,
		ghc,
		repo,
		optionsForRepo(config, ce.Repo.Owner.Login, ce.Repo.Name),
		&state{
			org:       ce.Repo.Owner.Login,
			repo:      ce.Repo.Name,
			number:    ce.Number,
			body:      ce.IssueBody,
			author:    ce.IssueAuthor.Login,
			assignees: ce.Assignees,
			htmlURL:   ce.IssueHTMLURL,
		},
	)
}

func handlePullRequestEvent(pc plugins.PluginClient, pre github.PullRequestEvent) error {
	if pre.Action != github.PullRequestActionOpened &&
		pre.Action != github.PullRequestActionReopened &&
		pre.Action != github.PullRequestActionSynchronize &&
		pre.Action != github.PullRequestActionLabeled {
		return nil
	}
	botName, err := pc.GitHubClient.BotName()
	if err != nil {
		return err
	}
	if pre.Action == github.PullRequestActionLabeled &&
		(pre.Label.Name != approvedLabel || pre.Sender.Login == botName || pre.PullRequest.State == "closed") {
		return nil
	}

	ro, err := pc.OwnersClient.LoadRepoOwners(pre.Repo.Owner.Login, pre.Repo.Name)
	if err != nil {
		return err
	}
	return handlePullRequest(pc.Logger, pc.GitHubClient, ro, pc.PluginConfig, &pre)
}

func handlePullRequest(log *logrus.Entry, ghc githubClient, repo approvers.RepoInterface, config *plugins.Configuration, pre *github.PullRequestEvent) error {
	return handle(
		log,
		ghc,
		repo,
		optionsForRepo(config, pre.Repo.Owner.Login, pre.Repo.Name),
		&state{
			org:       pre.Repo.Owner.Login,
			repo:      pre.Repo.Name,
			number:    pre.Number,
			body:      pre.PullRequest.Body,
			author:    pre.PullRequest.User.Login,
			assignees: pre.PullRequest.Assignees,
			htmlURL:   pre.PullRequest.HTMLURL,
		},
	)
}

// Returns associated issue, or 0 if it can't find any.
// This is really simple, and could be improved later.
func findAssociatedIssue(body string) int {
	match := associatedIssueRegex.FindStringSubmatch(body)
	if len(match) == 0 {
		return 0
	}
	v, err := strconv.Atoi(match[1])
	if err != nil {
		return 0
	}
	return v
}

// handle is the workhorse the will actually make updates to the PR.
// The algorithm goes as:
// - Initially, we build an approverSet
//   - Go through all comments in order of creation.
//		 - (Issue/PR comments, PR review comments, and PR review bodies are considered as comments)
//	 - If anyone said "/approve" or "/lgtm", add them to approverSet.
// - Then, for each file, we see if any approver of this file is in approverSet and keep track of files without approval
//   - An approver of a file is defined as:
//     - Someone listed as an "approver" in an OWNERS file in the files directory OR
//     - in one of the file's parent directorie
// - Iff all files have been approved, the bot will add the "approved" label.
// - Iff a cancel command is found, that reviewer will be removed from the approverSet
// 	and the munger will remove the approved label if it has been applied
func handle(log *logrus.Entry, ghc githubClient, repo approvers.RepoInterface, opts *plugins.Approve, pr *state) error {
	fetchErr := func(context string, err error) error {
		return fmt.Errorf("failed to get %s for %s/%s#%d: %v", context, pr.org, pr.repo, pr.number, err)
	}

	changes, err := ghc.GetPullRequestChanges(pr.org, pr.repo, pr.number)
	if err != nil {
		return fetchErr("PR file changes", err)
	}
	var filenames []string
	for _, change := range changes {
		filenames = append(filenames, change.Filename)
	}
	labels, err := ghc.GetIssueLabels(pr.org, pr.repo, pr.number)
	if err != nil {
		return fetchErr("issue labels", err)
	}
	hasApprovedLabel := false
	for _, label := range labels {
		if label.Name == approvedLabel {
			hasApprovedLabel = true
			break
		}
	}
	botName, err := ghc.BotName()
	if err != nil {
		return fetchErr("bot name", err)
	}
	issueComments, err := ghc.ListIssueComments(pr.org, pr.repo, pr.number)
	if err != nil {
		return fetchErr("issue comments", err)
	}
	reviewComments, err := ghc.ListPullRequestComments(pr.org, pr.repo, pr.number)
	if err != nil {
		return fetchErr("review comments", err)
	}
	reviews, err := ghc.ListReviews(pr.org, pr.repo, pr.number)
	if err != nil {
		return fetchErr("reviews", err)
	}

	approversHandler := approvers.NewApprovers(
		approvers.NewOwners(
			log,
			filenames,
			repo,
			int64(pr.number),
		),
	)
	approversHandler.AssociatedIssue = findAssociatedIssue(pr.body)
	approversHandler.RequireIssue = opts.IssueRequired
	approversHandler.ManuallyApproved = humanAddedApproved(ghc, log, pr.org, pr.repo, pr.number, botName, hasApprovedLabel)

	// Author implicitly approves their own PR if config allows it
	if opts.ImplicitSelfApprove {
		approversHandler.AddAuthorSelfApprover(pr.author, pr.htmlURL+"#", false)
	}

	commentsFromIssueComments := commentsFromIssueComments(issueComments)
	comments := append(commentsFromReviewComments(reviewComments), commentsFromIssueComments...)
	comments = append(comments, commentsFromReviews(reviews)...)
	sort.SliceStable(comments, func(i, j int) bool {
		return comments[i].CreatedAt.Before(comments[j].CreatedAt)
	})
	approveComments := filterComments(comments, approvalCommandMatcher(botName))
	addApprovers(&approversHandler, approveComments, pr.author)

	for _, user := range pr.assignees {
		approversHandler.AddAssignees(user.Login)
	}

	notifications := filterComments(commentsFromIssueComments, notificationMatcher(botName))
	latestNotification := getLast(notifications)
	newMessage := updateNotification(pr.org, pr.repo, latestNotification, approversHandler)
	if newMessage != nil {
		for _, notif := range notifications {
			if err := ghc.DeleteComment(pr.org, pr.repo, notif.ID); err != nil {
				log.WithError(err).Errorf("Failed to delete comment from %s/%s#%d, ID: %d.", pr.org, pr.repo, pr.number, notif.ID)
			}
		}
		if err := ghc.CreateComment(pr.org, pr.repo, pr.number, *newMessage); err != nil {
			log.WithError(err).Errorf("Failed to create comment on %s/%s#%d: %q.", pr.org, pr.repo, pr.number, *newMessage)
		}
	}

	if !approversHandler.IsApproved() {
		if hasApprovedLabel {
			if err := ghc.RemoveLabel(pr.org, pr.repo, pr.number, approvedLabel); err != nil {
				log.WithError(err).Errorf("Failed to remove %q label from %s/%s#%d.", approvedLabel, pr.org, pr.repo, pr.number)
			}
		}
	} else if !hasApprovedLabel {
		if err := ghc.AddLabel(pr.org, pr.repo, pr.number, approvedLabel); err != nil {
			log.WithError(err).Errorf("Failed to add %q label to %s/%s#%d.", approvedLabel, pr.org, pr.repo, pr.number)
		}
	}
	return nil
}

func humanAddedApproved(ghc githubClient, log *logrus.Entry, org, repo string, number int, botName string, hasLabel bool) func() bool {
	findOut := func() bool {
		if !hasLabel {
			return false
		}
		events, err := ghc.ListIssueEvents(org, repo, number)
		if err != nil {
			log.WithError(err).Errorf("Failed to list issue events for %s/%s#%d.", org, repo, number)
			return false
		}
		var lastAdded github.ListedIssueEvent
		for _, event := range events {
			// Only consider "approved" label added events.
			if event.Event != github.IssueActionLabeled || event.Label.Name != approvedLabel {
				continue
			}
			lastAdded = event
		}

		if lastAdded.Actor.Login == "" || lastAdded.Actor.Login == botName || lastAdded.Actor.Login == deprecatedBotName {
			return false
		}
		return true
	}

	var cache *bool
	return func() bool {
		if cache == nil {
			val := findOut()
			cache = &val
		}
		return *cache
	}
}

func approvalCommandMatcher(botName string) func(*comment) bool {
	return func(c *comment) bool {
		if c.Author == botName || c.Author == deprecatedBotName {
			return false
		}
		for _, match := range commandRegex.FindAllStringSubmatch(c.Body, -1) {
			cmd := strings.ToUpper(match[1])
			if cmd == lgtmCommand || cmd == approveCommand {
				return true
			}
		}
		return false
	}
}

func notificationMatcher(botName string) func(*comment) bool {
	return func(c *comment) bool {
		if c.Author != botName && c.Author != deprecatedBotName {
			return false
		}
		match := notificationRegex.FindStringSubmatch(c.Body)
		return len(match) > 0
	}
}

func updateNotification(org, project string, latestNotification *comment, approversHandler approvers.Approvers) *string {
	message := approvers.GetMessage(approversHandler, org, project)
	if message == nil || (latestNotification != nil && strings.Contains(latestNotification.Body, *message)) {
		return nil
	}
	return message
}

// addApprovers iterates through the list of comments on a PR
// and identifies all of the people that have said /approve and adds
// them to the Approvers.  The function uses the latest approve or cancel comment
// to determine the Users intention
func addApprovers(approversHandler *approvers.Approvers, approveComments []*comment, author string) {
	for _, c := range approveComments {
		if c.Author == "" {
			continue
		}
		for _, match := range commandRegex.FindAllStringSubmatch(c.Body, -1) {
			name := strings.ToUpper(match[1])
			if name != approveCommand && name != lgtmCommand {
				continue
			}
			args := strings.ToLower(strings.TrimSpace(match[2]))
			if args == cancelArgument {
				approversHandler.RemoveApprover(c.Author)
				continue
			}

			if c.Author == author {
				approversHandler.AddAuthorSelfApprover(
					c.Author,
					c.HTMLURL,
					args == noIssueArgument,
				)
			}

			if name == approveCommand {
				approversHandler.AddApprover(
					c.Author,
					c.HTMLURL,
					args == noIssueArgument,
				)
			} else {
				approversHandler.AddLGTMer(
					c.Author,
					c.HTMLURL,
					args == noIssueArgument,
				)
			}

		}
	}
}

// optionsForRepo gets the plugins.Approve struct that is applicable to the indicated repo.
func optionsForRepo(config *plugins.Configuration, org, repo string) *plugins.Approve {
	fullName := fmt.Sprintf("%s/%s", org, repo)
	for i := range config.Approve {
		if !strInSlice(org, config.Approve[i].Repos) && !strInSlice(fullName, config.Approve[i].Repos) {
			continue
		}
		return &config.Approve[i]
	}
	// Default to no issue required and no implicit self approval.
	return &plugins.Approve{}
}

func strInSlice(str string, slice []string) bool {
	for _, elem := range slice {
		if elem == str {
			return true
		}
	}
	return false
}

type comment struct {
	Body      string
	Author    string
	CreatedAt time.Time
	HTMLURL   string
	ID        int
}

func commentFromIssueComment(ic *github.IssueComment) *comment {
	if ic == nil {
		return nil
	}
	return &comment{
		Body:      ic.Body,
		Author:    ic.User.Login,
		CreatedAt: ic.CreatedAt,
		HTMLURL:   ic.HTMLURL,
		ID:        ic.ID,
	}
}

func commentsFromIssueComments(ics []github.IssueComment) []*comment {
	comments := []*comment{}
	for i := range ics {
		comments = append(comments, commentFromIssueComment(&ics[i]))
	}
	return comments
}

func commentFromReviewComment(rc *github.ReviewComment) *comment {
	if rc == nil {
		return nil
	}
	return &comment{
		Body:      rc.Body,
		Author:    rc.User.Login,
		CreatedAt: rc.CreatedAt,
		HTMLURL:   rc.HTMLURL,
		ID:        rc.ID,
	}
}

func commentsFromReviewComments(rcs []github.ReviewComment) []*comment {
	comments := []*comment{}
	for i := range rcs {
		comments = append(comments, commentFromReviewComment(&rcs[i]))
	}
	return comments
}

func commentFromReview(review *github.Review) *comment {
	if review == nil {
		return nil
	}
	return &comment{
		Body:      review.Body,
		Author:    review.User.Login,
		CreatedAt: review.SubmittedAt,
		HTMLURL:   review.HTMLURL,
		ID:        review.ID,
	}
}

func commentsFromReviews(reviews []github.Review) []*comment {
	comments := []*comment{}
	for i := range reviews {
		comments = append(comments, commentFromReview(&reviews[i]))
	}
	return comments
}

func filterComments(comments []*comment, filter func(*comment) bool) []*comment {
	var filtered []*comment
	for _, c := range comments {
		if filter(c) {
			filtered = append(filtered, c)
		}
	}
	return filtered
}

func getLast(cs []*comment) *comment {
	if len(cs) == 0 {
		return nil
	}
	return cs[len(cs)-1]
}
