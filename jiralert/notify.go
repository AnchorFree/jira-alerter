package jiralert

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"net/http"
	"crypto/sha1"
	"encoding/hex"

	"github.com/andygrunwald/go-jira"
	"github.com/anchorfree/jira-alerter/alertmanager"
	log "github.com/golang/glog"
	"github.com/trivago/tgo/tcontainer"
)

// Receiver wraps a JIRA client corresponding to a specific Alertmanager receiver, with its configuration and templates.
type Receiver struct {
	conf   *ReceiverConfig
	tmpl   *Template
	client *jira.Client
}

// NewReceiver creates a Receiver using the provided configuration and template.
func NewReceiver(c *ReceiverConfig, t *Template) (*Receiver, error) {
	client, err := jira.NewClient(http.DefaultClient, c.APIURL)
	if err != nil {
		return nil, err
	}
	client.Authentication.SetBasicAuth(c.User, string(c.Password))

	return &Receiver{conf: c, tmpl: t, client: client}, nil
}

// Notify implements the Notifier interface.
func (r *Receiver) Notify(data *alertmanager.Data) (bool, error) {
	project := r.tmpl.Execute(r.conf.Project, data)
	// check errors from r.tmpl.Execute()
	if r.tmpl.err != nil {
		return false, r.tmpl.err
	}
	// Looks like an ALERT metric name, with spaces removed.
	var issueLabel []string
	var alertHash string
	addIssueLabel := false

	// Add Labels
	if r.conf.AddLabels {
		addIssueLabel = true
		issueLabel = toIssueLabel(data.GroupLabels)
		issueLabel = append([]string{"ALERT"}, issueLabel...)
	}

	if r.conf.AlertHash != "" {
		sha1_Bytes_alertHash := sha1.Sum([]byte(r.tmpl.Execute(r.conf.AlertHash, data)))
		alertHash = hex.EncodeToString(sha1_Bytes_alertHash[:])
	} else {
		alertHash = ""
	}

        issue, retry, err := r.search(project, r.tmpl.Execute(r.conf.Summary, data), issueLabel, addIssueLabel, alertHash)
        if err != nil {
                return retry, err
        }


	if issue != nil {
		// The set of JIRA status categories is fixed, this is a safe check to make.
		if issue.Fields.Status.StatusCategory.Key != "done" {
			// Issue is in a "to do" or "in progress" state, all done here.
			log.V(1).Infof("Issue %s for %s is unresolved, nothing to do", issue.Key, issueLabel)
			return false, nil
		}
		if r.conf.WontFixResolution != "" && issue.Fields.Resolution.Name == r.conf.WontFixResolution {
			// Issue is resolved as "Won't Fix" or equivalent, log a message just in case.
			log.Infof("Issue %s for %s is resolved as %q, not reopening", issue.Key, issueLabel, issue.Fields.Resolution.Name)
			return false, nil
		}
		log.Infof("Issue %s for %s was resolved, reopening", issue.Key, issueLabel)
		return r.reopen(issue.Key)
	}


	log.Infof("No issue matching %s found, creating new issue", issueLabel)
	issue = &jira.Issue{
		Fields: &jira.IssueFields{
			Project:     jira.Project{Key: project},
			Type:        jira.IssueType{Name: r.tmpl.Execute(r.conf.IssueType, data)},
			Description: (fmt.Sprintf("%s\n\nalert_hash=%s", r.tmpl.Execute(r.conf.Description, data), alertHash)),
			Summary:     r.tmpl.Execute(r.conf.Summary, data),
			Labels: issueLabel,
			Unknowns: tcontainer.NewMarshalMap(),
		},
	}
	if r.conf.Priority != "" {
		issue.Fields.Priority = &jira.Priority{Name: r.tmpl.Execute(r.conf.Priority, data)}
	}
	for key, value := range r.conf.Fields {
		issue.Fields.Unknowns[key] = r.tmpl.Execute(fmt.Sprint(value), data)
	}
	// check errors from r.tmpl.Execute()
	if r.tmpl.err != nil {
		return false, r.tmpl.err
	}
	retry, err = r.create(issue)
	if err != nil {
		log.Infof("Issue created: key=%s ID=%s", issue.Key, issue.ID)
	}
	return retry, err
}

// toIssueLabel returns the group labels in the form of an ALERT metric name, with all spaces removed.
func toIssueLabel(groupLabels alertmanager.KV) []string {
	var l []string
	for _, p := range groupLabels.SortedPairs() {
		l = append(l, p.Value)
	}
	return l
}

func (r *Receiver) search(project, summary string, il []string, addIssueLabel bool, alertHash string) (*jira.Issue, bool, error) {
	var search_labels string
	var query string

	if alertHash != "" {
		query = fmt.Sprintf("project=%s and description~\"alert_hash=%s\" order by key", project, alertHash)
	} else {
		if addIssueLabel {
			buf := bytes.NewBufferString("")
			for _, l := range il {
				buf.WriteString(fmt.Sprintf("and labels=%q ", l))
			}
			buf.Truncate(buf.Len() - 1)
			search_labels = buf.String()

			query = fmt.Sprintf("project=%s %s order by key", project, search_labels)
		} else {
			query = fmt.Sprintf("project=%s order by key", project)
		}
	}

	options := &jira.SearchOptions{
		Fields:     []string{"summary", "status", "resolution"},
		MaxResults: 50,
	}
	log.V(1).Infof("search: query=%v options=%+v", query, options)
	issues, resp, err := r.client.Issue.Search(query, options)
	if err != nil {
		retry, err := handleJiraError(resp, err)
		return nil, retry, err
	}
	if len(issues) > 0 {
		if len(issues) > 1 {
			// Swallow it, but log an error.
			log.Errorf("More than one issue matched %s, will only update first: %+v", query, issues)
		}
		log.V(1).Infof("  found: %+v", issues[0])
		return &issues[0], false, nil
	}
	log.V(1).Infof("  no results")
	return nil, false, nil
}

func (r *Receiver) reopen(issueKey string) (bool, error) {
	transitions, resp, err := r.client.Issue.GetTransitions(issueKey)
	if err != nil {
		return handleJiraError(resp, err)
	}
	for _, t := range transitions {
		if t.Name == r.conf.ReopenState {
			log.V(1).Infof("reopen: issueKey=%v transitionID=%v", issueKey, t.ID)
			resp, err = r.client.Issue.DoTransition(issueKey, t.ID)
			if err != nil {
				return handleJiraError(resp, err)
			}
			log.V(1).Infof("  done")
			return false, nil
		}
	}
	return false, fmt.Errorf("JIRA state %q does not exist or no transition possible for %s", r.conf.ReopenState, issueKey)
}

func (r *Receiver) create(issue *jira.Issue) (bool, error) {
	log.V(1).Infof("create: issue=%v", *issue)
	issue, resp, err := r.client.Issue.Create(issue)
	if err != nil {
		return handleJiraError(resp, err)
	}

	log.V(1).Infof("  done: key=%s ID=%s", issue.Key, issue.ID)
	return false, nil
}

func handleJiraError(resp *jira.Response, err error) (bool, error) {
	log.V(1).Infof("handleJiraError: err=%s, req=%s", err, resp.Request.URL)
	if resp != nil && resp.StatusCode/100 != 2 {
		retry := resp.StatusCode == 500 || resp.StatusCode == 503
		body, _ := ioutil.ReadAll(resp.Body)
		// go-jira error message is not particularly helpful, replace it
		return retry, fmt.Errorf("JIRA request %s returned status %s, body %q", resp.Request.URL, resp.Status, string(body))
	}
	return false, err
}
