package cmd

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/reaper"
	"github.com/steveyegge/gastown/internal/workspace"
)

var reaperReconcileCmd = &cobra.Command{
	Use:   "reconcile-anomalies",
	Short: "Reconcile Reaper anomalies with durable escalations",
	RunE:  runReaperReconcileAnomalies,
}

type anomalyReconcileDeps struct {
	now    func() time.Time
	list   func() ([]*beads.Issue, error)
	create func(string, *beads.EscalationFields) (*beads.Issue, error)
	close  func(string, string, string) error
	send   func(*beads.Issue, reaper.Anomaly) error
	mark   func(*beads.Issue) error
	wait   func() error
}

type anomalyReconcileResult struct {
	Created  int `json:"created"`
	Resolved int `json:"resolved"`
}

func reconcileAnomalyScans(scans []reaper.AnomalyScan, deps anomalyReconcileDeps) (anomalyReconcileResult, error) {
	issues, err := deps.list()
	if err != nil {
		return anomalyReconcileResult{}, errors.New("list anomaly escalations: operation failed")
	}
	occurrences := make([]reaper.AnomalyOccurrence, 0, len(issues))
	issuesByFingerprint := make(map[string][]*beads.Issue)
	for _, issue := range issues {
		fields := beads.ParseEscalationFields(issue.Description)
		if fields.Fingerprint == "" || fields.AnomalyFamily == "" || fields.AnomalyScope == "" {
			continue
		}
		createdAt, _ := time.Parse(time.RFC3339, issue.CreatedAt)
		occurrences = append(occurrences, reaper.AnomalyOccurrence{
			ID:          issue.ID,
			Fingerprint: fields.Fingerprint,
			Family:      fields.AnomalyFamily,
			Scope:       fields.AnomalyScope,
			Status:      issue.Status,
			CreatedAt:   createdAt,
		})
		if issue.Status == "open" {
			issuesByFingerprint[fields.Fingerprint] = append(issuesByFingerprint[fields.Fingerprint], issue)
		}
	}

	actions, err := reaper.ReconcileAnomalies(scans, occurrences)
	if err != nil {
		return anomalyReconcileResult{}, errors.New("invalid anomaly reconciliation input")
	}
	current := make(map[string]reaper.Anomaly)
	for _, scan := range scans {
		if !scan.Complete {
			continue
		}
		for _, anomaly := range scan.Anomalies {
			fingerprint, err := reaper.FingerprintAnomaly(anomaly)
			if err != nil {
				return anomalyReconcileResult{}, errors.New("invalid anomaly reconciliation input")
			}
			current[fingerprint] = anomaly
		}
	}
	fingerprints := make([]string, 0, len(current))
	for fingerprint := range current {
		fingerprints = append(fingerprints, fingerprint)
	}
	sort.Strings(fingerprints)
	for _, fingerprint := range fingerprints {
		matching := issuesByFingerprint[fingerprint]
		if len(matching) == 0 {
			continue
		}
		sort.Slice(matching, func(i, j int) bool {
			if matching[i].CreatedAt == matching[j].CreatedAt {
				return matching[i].ID < matching[j].ID
			}
			return matching[i].CreatedAt < matching[j].CreatedAt
		})
		if err := sendAnomalyMail(deps, matching[len(matching)-1], current[fingerprint]); err != nil {
			return anomalyReconcileResult{}, err
		}
	}
	result := anomalyReconcileResult{}
	for _, action := range actions {
		switch action.Kind {
		case reaper.ReconcileResolve:
			if err := deps.close(action.OccurrenceID, "reaper", "anomaly no longer present"); err != nil {
				return result, errors.New("resolve anomaly escalation: operation failed")
			}
			result.Resolved++
		case reaper.ReconcileCreate:
			title := fmt.Sprintf("Reaper anomaly: %s (%s)", action.Anomaly.Type, action.Anomaly.Scope)
			reason := action.Anomaly.Message
			if reason == "" {
				reason = action.Anomaly.Remediation
			}
			issue, err := deps.create(title, &beads.EscalationFields{
				Severity:           "medium",
				Reason:             reason,
				Source:             "reaper",
				EscalatedBy:        "reaper",
				EscalatedAt:        deps.now().UTC().Format(time.RFC3339),
				Fingerprint:        action.Fingerprint,
				AnomalyFamily:      action.Family,
				AnomalyScope:       action.Anomaly.Scope,
				PreviousOccurrence: action.PreviousOccurrence,
			})
			if err != nil {
				return result, errors.New("create anomaly escalation: operation failed")
			}
			if err := sendAnomalyMail(deps, issue, action.Anomaly); err != nil {
				return result, err
			}
			result.Created++
		}
	}
	return result, nil
}

func sendAnomalyMail(deps anomalyReconcileDeps, issue *beads.Issue, anomaly reaper.Anomaly) error {
	if beads.ParseEscalationFields(issue.Description).AnomalyMailStored {
		return nil
	}
	if err := deps.send(issue, anomaly); err != nil {
		return errors.New("store anomaly escalation mail: operation failed")
	}
	if err := deps.mark(issue); err != nil {
		return errors.New("mark anomaly escalation mail stored: operation failed")
	}
	if err := deps.wait(); err != nil {
		return errors.New("anomaly escalation notification: delivery not confirmed")
	}
	return nil
}

func runReaperReconcileAnomalies(cmd *cobra.Command, _ []string) error {
	maxAge, err := time.ParseDuration(reaperMaxAge)
	if err != nil {
		return fmt.Errorf("invalid --max-age: %w", err)
	}
	purgeAge, err := time.ParseDuration(reaperPurgeAge)
	if err != nil {
		return fmt.Errorf("invalid --purge-age: %w", err)
	}
	mailAge, err := time.ParseDuration(reaperMailAge)
	if err != nil {
		return fmt.Errorf("invalid --mail-age: %w", err)
	}
	staleAge, err := time.ParseDuration(reaperStaleAge)
	if err != nil {
		return fmt.Errorf("invalid --stale-age: %w", err)
	}

	var scans []reaper.AnomalyScan
	var incomplete []string
	for i, dbName := range reaperDatabaseNames() {
		if err := waitBeforeReaperDatabase(i); err != nil {
			return err
		}
		if err := reaper.ValidateDBName(dbName); err != nil {
			incomplete = append(incomplete, "invalid scope")
			continue
		}
		scan := reaper.AnomalyScan{Scope: dbName}
		db, err := reaper.OpenDB(reaperHost, reaperPort, dbName, 10*time.Second, 10*time.Second)
		if err != nil {
			incomplete = append(incomplete, dbName+": open failed")
			scans = append(scans, scan)
			continue
		}
		ok, schemaErr := reaper.HasReaperSchema(db)
		if schemaErr != nil || !ok {
			db.Close()
			if schemaErr != nil {
				incomplete = append(incomplete, dbName+": schema check failed")
			} else {
				incomplete = append(incomplete, dbName+": reaper schema unavailable")
			}
			scans = append(scans, scan)
			continue
		}
		result, scanErr := reaper.Scan(db, dbName, maxAge, purgeAge, mailAge, staleAge)
		db.Close()
		if scanErr != nil {
			incomplete = append(incomplete, dbName+": scan failed")
			scans = append(scans, scan)
			continue
		}
		scan.Complete = true
		scan.Anomalies = result.Anomalies
		scans = append(scans, scan)
	}

	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	bd := beads.New(beads.ResolveBeadsDir(townRoot))
	escalationConfig, err := config.LoadOrCreateEscalationConfig(config.EscalationConfigPath(townRoot))
	if err != nil {
		return fmt.Errorf("loading escalation config: %w", err)
	}
	targets := extractMailTargetsFromActions(escalationConfig.GetRouteForSeverity(config.SeverityMedium))
	router := mail.NewRouter(townRoot)
	result, err := reconcileAnomalyScans(scans, anomalyReconcileDeps{
		now:    time.Now,
		list:   bd.ListEscalationOccurrences,
		create: bd.CreateEscalationBead,
		close:  bd.CloseEscalation,
		send: func(issue *beads.Issue, anomaly reaper.Anomaly) error {
			return sendReaperAnomalyMail(issue, anomaly, targets,
				func(target, threadID string) ([]*mail.Message, error) {
					return mail.NewMailboxFromAddress(target, townRoot).ListByThread(threadID)
				},
				router.Send,
			)
		},
		mark: func(issue *beads.Issue) error {
			fields := beads.ParseEscalationFields(issue.Description)
			fields.AnomalyMailStored = true
			description := beads.FormatEscalationDescription(issue.Title, fields)
			if err := bd.Update(issue.ID, beads.UpdateOptions{Description: &description}); err != nil {
				return err
			}
			issue.Description = description
			return nil
		},
		wait: router.WaitPendingNotifications,
	})
	if err != nil {
		return err
	}
	if reaperJSON {
		fmt.Println(reaper.FormatJSON(result))
	} else {
		fmt.Printf("Reaper anomaly reconciliation: %d created, %d resolved\n", result.Created, result.Resolved)
	}
	if len(incomplete) > 0 {
		return fmt.Errorf("incomplete anomaly scopes preserved: %s", strings.Join(incomplete, "; "))
	}
	return nil
}

func sendReaperAnomalyMail(
	issue *beads.Issue,
	anomaly reaper.Anomaly,
	targets []string,
	list func(target, threadID string) ([]*mail.Message, error),
	send func(*mail.Message) error,
) error {
	for _, target := range targets {
		stored, err := list(target, issue.ID)
		if err != nil {
			return err
		}
		found := false
		for _, message := range stored {
			if isStoredReaperNotice(message, target, issue.ID) {
				found = true
				break
			}
		}
		if found {
			continue
		}
		if err := send(&mail.Message{
			From:     "reaper",
			To:       target,
			Subject:  fmt.Sprintf("[MEDIUM] Reaper anomaly: %s (%s)", anomaly.Type, anomaly.Scope),
			Body:     formatEscalationMailBody(issue.ID, config.SeverityMedium, anomaly.Message, "reaper", ""),
			Type:     mail.TypeEscalation,
			Priority: mail.PriorityNormal,
			ThreadID: issue.ID,
		}); err != nil {
			return err
		}
	}
	return nil
}

func isStoredReaperNotice(message *mail.Message, target, threadID string) bool {
	if message == nil || message.Validate() != nil {
		return false
	}
	return mail.AddressToIdentity(message.From) == "reaper" &&
		mail.AddressToIdentity(message.To) == mail.AddressToIdentity(target) &&
		message.Type == mail.TypeEscalation &&
		message.ThreadID == threadID
}
