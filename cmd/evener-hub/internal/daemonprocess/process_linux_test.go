package daemonprocess

import (
	"encoding/binary"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestLinuxLockEvidence(t *testing.T) {
	for _, tt := range []struct {
		name, text string
		want       bool
	}{
		{"exclusive owner", "flags:\t02100002\nlock:\t1: FLOCK  ADVISORY  WRITE  123  08:01:55 0 EOF\n", true},
		{"foreign owner", "flags:\t02100002\nlock:\t1: FLOCK  ADVISORY  WRITE  124  08:01:55 0 EOF\n", false},
		{"shared lock", "flags:\t02100002\nlock:\t1: FLOCK  ADVISORY  READ  123  08:01:55 0 EOF\n", false},
		{"read only", "flags:\t02100000\nlock:\t1: FLOCK  ADVISORY  WRITE  123  08:01:55 0 EOF\n", false},
		{"different inode", "flags:\t02100002\nlock:\t1: FLOCK  ADVISORY  WRITE  123  08:01:56 0 EOF\n", false},
		{"no lock", "flags:\t02100002\n", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := linuxOwnsLock([]byte(tt.text), 123, unix.Mkdev(8, 1), 55); got != tt.want {
				t.Fatalf("owns lock = %v", got)
			}
		})
	}
}

func TestLinuxClockTicks(t *testing.T) {
	buf := make([]byte, 32)
	binary.NativeEndian.PutUint64(buf, 17)
	binary.NativeEndian.PutUint64(buf[8:], 100)
	if ticks, err := linuxClockTicks(buf, 8); err != nil || ticks != 100 {
		t.Fatalf("ticks=%d err=%v", ticks, err)
	}
	if _, err := linuxClockTicks(buf[:8], 8); err == nil {
		t.Fatal("truncated auxiliary vector accepted")
	}
}

// The fixture publishes its rendezvous after the kernel start tick has ended.
// A Go test helper can start inside that tick, unlike a fully initialized daemon.
func fixtureRendezvousTime(t *testing.T, target Target) time.Time {
	t.Helper()
	h, err := openLinuxProcess(target.PID)
	if err != nil {
		t.Fatal(err)
	}
	defer h.close()
	facts, err := h.inspect(target)
	if err != nil {
		t.Fatal(err)
	}
	if delay := time.Until(facts.startedAt); delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		<-timer.C
	}
	return time.Now()
}
