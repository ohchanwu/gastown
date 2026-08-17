package dog

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/mail"
)

type pluginMailArchiver interface {
	List() ([]*mail.Message, error)
	Archive(string) error
}

// archivePluginAssignmentMails archives only the durable dispatch thread owned
// by one exact dog assignment. A reused dog name is deliberately insufficient
// custody because a replacement may already own different instructions.
func (m *Manager) archivePluginAssignmentMails(dogName, work string, startedAt time.Time) error {
	dispatchThreadID := AssignmentThreadID(dogName, work, startedAt)
	if dispatchThreadID == "" {
		return errors.New("exact dog assignment mail token is unavailable")
	}

	dogAddress := fmt.Sprintf("deacon/dogs/%s", dogName)
	router := mail.NewRouterWithTownRoot(m.townRoot, m.townRoot)
	mailbox, err := router.GetMailbox(dogAddress)
	if err != nil {
		return fmt.Errorf("opening dog mailbox: %w", err)
	}
	_, err = archivePluginDispatchMails(mailbox, dogAddress, dispatchThreadID)
	return err
}

func archivePluginDispatchMails(mailbox pluginMailArchiver, dogAddress, dispatchThreadID string) (int, error) {
	messages, err := mailbox.List()
	if err != nil {
		return 0, fmt.Errorf("listing dog mailbox: %w", err)
	}

	archived := 0
	for _, msg := range messages {
		if !matchesPluginDispatchMail(msg, dogAddress, dispatchThreadID) {
			continue
		}
		if err := mailbox.Archive(msg.ID); err != nil {
			return archived, fmt.Errorf("archiving exact dog dispatch mail %s: %w", msg.ID, err)
		}
		archived++
	}
	return archived, nil
}

func matchesPluginDispatchMail(msg *mail.Message, dogAddress, dispatchThreadID string) bool {
	if msg == nil || dispatchThreadID == "" || msg.ThreadID != dispatchThreadID {
		return false
	}
	if !strings.HasPrefix(msg.Subject, "Plugin: ") {
		return false
	}
	if mail.AddressToIdentity(msg.To) != mail.AddressToIdentity(dogAddress) {
		return false
	}
	sender := mail.AddressToIdentity(msg.From)
	return sender == "deacon/" || sender == "daemon"
}
