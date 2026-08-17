package dog

import (
	"errors"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/mail"
)

type fakePluginMailArchiver struct {
	messages   []*mail.Message
	listErr    error
	archiveErr error
	archived   []string
}

func (f *fakePluginMailArchiver) List() ([]*mail.Message, error) {
	return f.messages, f.listErr
}

func (f *fakePluginMailArchiver) Archive(id string) error {
	if f.archiveErr != nil {
		return f.archiveErr
	}
	f.archived = append(f.archived, id)
	return nil
}

func TestPluginMailCleanupMatchesOnlyCompletedAssignment(t *testing.T) {
	started := time.Now().UTC().Round(0)
	oldThread := AssignmentThreadID("alpha", "plugin:old", started)
	newThread := AssignmentThreadID("alpha", "plugin:new", started.Add(time.Nanosecond))
	dogAddress := "deacon/dogs/alpha"
	oldMail := &mail.Message{
		From:     "deacon/",
		To:       dogAddress,
		Subject:  "Plugin: reaper",
		ThreadID: oldThread,
	}
	replacementMail := *oldMail
	replacementMail.ThreadID = newThread

	if !matchesPluginDispatchMail(oldMail, dogAddress, oldThread) {
		t.Fatal("completed assignment mail did not match its exact dispatch thread")
	}
	if matchesPluginDispatchMail(&replacementMail, dogAddress, oldThread) {
		t.Fatal("stale closeout matched replacement assignment mail")
	}
	if oldThread == "" || oldThread == newThread {
		t.Fatalf("dispatch thread tokens are not assignment-specific: old=%q new=%q", oldThread, newThread)
	}
}

func TestArchivePluginDispatchMailsReportsListAndArchiveFailures(t *testing.T) {
	dogAddress := "deacon/dogs/alpha"
	thread := AssignmentThreadID("alpha", "plugin:reaper", time.Now().UTC().Round(0))
	wantErr := errors.New("mail store unavailable")

	if _, err := archivePluginDispatchMails(&fakePluginMailArchiver{listErr: wantErr}, dogAddress, thread); !errors.Is(err, wantErr) {
		t.Fatalf("list failure = %v, want %v", err, wantErr)
	}
	mailbox := &fakePluginMailArchiver{
		messages: []*mail.Message{{
			ID:       "mail-old",
			From:     "daemon",
			To:       dogAddress,
			Subject:  "Plugin: reaper",
			ThreadID: thread,
		}},
		archiveErr: wantErr,
	}
	if _, err := archivePluginDispatchMails(mailbox, dogAddress, thread); !errors.Is(err, wantErr) {
		t.Fatalf("archive failure = %v, want %v", err, wantErr)
	}
	if len(mailbox.archived) != 0 {
		t.Fatalf("failed archive recorded success: %v", mailbox.archived)
	}
}
