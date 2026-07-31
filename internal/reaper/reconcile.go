package reaper

import (
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"
	"time"
)

const anomalyFingerprintVersion = "reaper-anomaly/v1"

const (
	ReconcileCreate  = "create"
	ReconcileResolve = "resolve"
)

// AnomalyScan is the anomaly result for one database scope.
// Incomplete scopes are never used to resolve prior occurrences.
type AnomalyScan struct {
	Scope     string
	Complete  bool
	Anomalies []Anomaly
}

// AnomalyOccurrence is the durable lifecycle state needed by reconciliation.
type AnomalyOccurrence struct {
	ID          string
	Fingerprint string
	Family      string
	Scope       string
	Status      string
	CreatedAt   time.Time
}

// ReconcileAction is a deterministic lifecycle mutation.
type ReconcileAction struct {
	Kind               string
	OccurrenceID       string
	Fingerprint        string
	Family             string
	PreviousOccurrence string
	Anomaly            Anomaly
}

// FingerprintAnomaly returns a stable identifier for an anomaly occurrence.
// Display-only fields such as Message and Count are deliberately excluded.
func FingerprintAnomaly(anomaly Anomaly) (string, error) {
	normalized, err := normalizeAnomaly(anomaly)
	if err != nil {
		return "", err
	}
	return hashAnomaly(anomalyFingerprintVersion, "reaper-anomaly:v1:", normalized, true), nil
}

func fingerprintAnomalyFamily(anomaly Anomaly) (string, error) {
	normalized, err := normalizeAnomaly(anomaly)
	if err != nil {
		return "", err
	}
	return hashAnomaly("reaper-anomaly-family/v1", "reaper-anomaly-family:v1:", normalized, false), nil
}

func normalizeAnomaly(anomaly Anomaly) (Anomaly, error) {
	typeName := strings.TrimSpace(anomaly.Type)
	scope := strings.TrimSpace(anomaly.Scope)
	remediation := strings.TrimSpace(anomaly.Remediation)
	for name, value := range map[string]string{
		"type":        typeName,
		"scope":       scope,
		"remediation": remediation,
	} {
		if !validDBName.MatchString(value) {
			return Anomaly{}, fmt.Errorf("invalid anomaly %s: %q", name, value)
		}
	}

	ids := make([]string, 0, len(anomaly.AffectedIDs))
	seen := make(map[string]struct{}, len(anomaly.AffectedIDs))
	for _, id := range anomaly.AffectedIDs {
		id = strings.TrimSpace(id)
		if id == "" || strings.ContainsAny(id, "\r\n") {
			return Anomaly{}, fmt.Errorf("invalid affected ID: %q", id)
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	anomaly.Type = typeName
	anomaly.Scope = scope
	anomaly.Remediation = remediation
	anomaly.AffectedIDs = ids
	return anomaly, nil
}

func hashAnomaly(version, prefix string, anomaly Anomaly, includeIDs bool) string {
	var canonical strings.Builder
	fmt.Fprintf(&canonical, "%s\n%s\n%s\n%s\n", version, anomaly.Type, anomaly.Scope, anomaly.Remediation)
	if includeIDs {
		for _, id := range anomaly.AffectedIDs {
			fmt.Fprintf(&canonical, "%s\n", id)
		}
	}
	sum := sha256.Sum256([]byte(canonical.String()))
	return fmt.Sprintf("%s%x", prefix, sum)
}

// ReconcileAnomalies plans lifecycle mutations from complete anomaly scans.
func ReconcileAnomalies(scans []AnomalyScan, occurrences []AnomalyOccurrence) ([]ReconcileAction, error) {
	active := make(map[string]bool)
	activeByFingerprint := make(map[string][]AnomalyOccurrence)
	activeByFamily := make(map[string][]AnomalyOccurrence)
	closedByFingerprint := make(map[string][]AnomalyOccurrence)
	completeScopes := make(map[string]bool)
	currentFingerprints := make(map[string]bool)
	resolvedIDs := make(map[string]bool)
	for _, occurrence := range occurrences {
		if occurrence.Status == "open" {
			active[occurrence.Fingerprint] = true
			activeByFingerprint[occurrence.Fingerprint] = append(activeByFingerprint[occurrence.Fingerprint], occurrence)
			activeByFamily[occurrence.Family] = append(activeByFamily[occurrence.Family], occurrence)
		} else if occurrence.Status == "closed" {
			closedByFingerprint[occurrence.Fingerprint] = append(closedByFingerprint[occurrence.Fingerprint], occurrence)
		}
	}

	orderedScans := append([]AnomalyScan(nil), scans...)
	sort.Slice(orderedScans, func(i, j int) bool { return orderedScans[i].Scope < orderedScans[j].Scope })
	var actions []ReconcileAction
	for _, scan := range orderedScans {
		if !validDBName.MatchString(scan.Scope) {
			return nil, fmt.Errorf("invalid anomaly scan scope: %q", scan.Scope)
		}
		if !scan.Complete {
			continue
		}
		completeScopes[scan.Scope] = true
		normalizedAnomalies := make([]Anomaly, 0, len(scan.Anomalies))
		for _, anomaly := range scan.Anomalies {
			normalized, err := normalizeAnomaly(anomaly)
			if err != nil {
				return nil, err
			}
			if normalized.Scope != scan.Scope {
				return nil, fmt.Errorf("anomaly scope %q does not match scan scope %q", normalized.Scope, scan.Scope)
			}
			normalizedAnomalies = append(normalizedAnomalies, normalized)
		}
		sort.Slice(normalizedAnomalies, func(i, j int) bool {
			left, _ := FingerprintAnomaly(normalizedAnomalies[i])
			right, _ := FingerprintAnomaly(normalizedAnomalies[j])
			return left < right
		})
		for _, normalized := range normalizedAnomalies {
			fingerprint, _ := FingerprintAnomaly(normalized)
			if currentFingerprints[fingerprint] {
				continue
			}
			currentFingerprints[fingerprint] = true
			if active[fingerprint] {
				matching := activeByFingerprint[fingerprint]
				keeper := latestOccurrence(matching)
				if len(matching) > 1 {
					sort.Slice(matching, func(i, j int) bool { return matching[i].ID < matching[j].ID })
					for _, occurrence := range matching {
						if occurrence.ID == keeper.ID {
							continue
						}
						actions = append(actions, ReconcileAction{
							Kind:         ReconcileResolve,
							OccurrenceID: occurrence.ID,
						})
						resolvedIDs[occurrence.ID] = true
					}
				}
				for _, occurrence := range activeByFamily[keeper.Family] {
					if occurrence.Fingerprint == fingerprint || resolvedIDs[occurrence.ID] {
						continue
					}
					actions = append(actions, ReconcileAction{
						Kind:         ReconcileResolve,
						OccurrenceID: occurrence.ID,
					})
					resolvedIDs[occurrence.ID] = true
				}
				continue
			}
			family, _ := fingerprintAnomalyFamily(normalized)
			previous := ""
			familyActive := activeByFamily[family]
			var superseded []ReconcileAction
			if len(familyActive) > 0 {
				sort.Slice(familyActive, func(i, j int) bool { return familyActive[i].ID < familyActive[j].ID })
				for _, occurrence := range familyActive {
					superseded = append(superseded, ReconcileAction{
						Kind:         ReconcileResolve,
						OccurrenceID: occurrence.ID,
					})
					delete(active, occurrence.Fingerprint)
					resolvedIDs[occurrence.ID] = true
				}
				previous = latestOccurrence(familyActive).ID
				delete(activeByFamily, family)
			} else if closed := closedByFingerprint[fingerprint]; len(closed) > 0 {
				previous = latestOccurrence(closed).ID
			}
			actions = append(actions, ReconcileAction{
				Kind:               ReconcileCreate,
				Fingerprint:        fingerprint,
				Family:             family,
				PreviousOccurrence: previous,
				Anomaly:            normalized,
			})
			actions = append(actions, superseded...)
			active[fingerprint] = true
		}
	}
	openOccurrences := append([]AnomalyOccurrence(nil), occurrences...)
	sort.Slice(openOccurrences, func(i, j int) bool { return openOccurrences[i].ID < openOccurrences[j].ID })
	for _, occurrence := range openOccurrences {
		if occurrence.Status != "open" || !completeScopes[occurrence.Scope] ||
			currentFingerprints[occurrence.Fingerprint] || resolvedIDs[occurrence.ID] {
			continue
		}
		actions = append(actions, ReconcileAction{
			Kind:         ReconcileResolve,
			OccurrenceID: occurrence.ID,
		})
	}
	return actions, nil
}

func latestOccurrence(occurrences []AnomalyOccurrence) AnomalyOccurrence {
	latest := occurrences[0]
	for _, occurrence := range occurrences[1:] {
		if occurrence.CreatedAt.After(latest.CreatedAt) ||
			(occurrence.CreatedAt.Equal(latest.CreatedAt) && occurrence.ID > latest.ID) {
			latest = occurrence
		}
	}
	return latest
}
