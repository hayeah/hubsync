package hubsync

import (
	"testing"
	"time"
)

func TestBroadcasterSubscribeAndBroadcast(t *testing.T) {
	bc := NewBroadcaster()

	ch1 := bc.Subscribe()
	ch2 := bc.Subscribe()

	entry := ChangeEntry{
		Version: 1,
		Path:    "file.txt",
		Op:      OpCreate,
	}
	bc.Broadcast(entry)

	// Both subscribers should receive the entry
	select {
	case got := <-ch1:
		if got.Path != "file.txt" {
			t.Errorf("ch1 path: got %q, want %q", got.Path, "file.txt")
		}
	case <-time.After(time.Second):
		t.Fatal("ch1 timeout")
	}

	select {
	case got := <-ch2:
		if got.Path != "file.txt" {
			t.Errorf("ch2 path: got %q, want %q", got.Path, "file.txt")
		}
	case <-time.After(time.Second):
		t.Fatal("ch2 timeout")
	}
}

func TestBroadcasterUnsubscribe(t *testing.T) {
	bc := NewBroadcaster()

	ch1 := bc.Subscribe()
	ch2 := bc.Subscribe()

	bc.Unsubscribe(ch1)

	entry := ChangeEntry{Version: 1, Path: "file.txt", Op: OpCreate}
	bc.Broadcast(entry)

	// ch1 should be closed
	_, ok := <-ch1
	if ok {
		t.Error("expected ch1 to be closed after unsubscribe")
	}

	// ch2 should still receive
	select {
	case got := <-ch2:
		if got.Path != "file.txt" {
			t.Errorf("ch2 path: got %q, want %q", got.Path, "file.txt")
		}
	case <-time.After(time.Second):
		t.Fatal("ch2 timeout")
	}
}

func TestBroadcasterSlowSubscriberDrop(t *testing.T) {
	bc := NewBroadcaster()

	ch := bc.Subscribe() // buffer size 256

	// Fill the buffer
	for i := 0; i < 256; i++ {
		bc.Broadcast(ChangeEntry{Version: int64(i + 1), Path: "fill.txt", Op: OpCreate})
	}

	// This broadcast should be dropped (non-blocking send)
	bc.Broadcast(ChangeEntry{Version: 257, Path: "dropped.txt", Op: OpCreate})

	// Drain and check we got exactly 256
	count := 0
	for {
		select {
		case <-ch:
			count++
		default:
			goto done
		}
	}
done:
	if count != 256 {
		t.Errorf("received %d events, want 256", count)
	}
}

func TestBroadcasterNoSubscribers(t *testing.T) {
	bc := NewBroadcaster()
	// Should not panic
	bc.Broadcast(ChangeEntry{Version: 1, Path: "nobody.txt", Op: OpCreate})
}

func TestBroadcasterMultipleEvents(t *testing.T) {
	bc := NewBroadcaster()
	ch := bc.Subscribe()

	events := []ChangeEntry{
		{Version: 1, Path: "a.txt", Op: OpCreate},
		{Version: 2, Path: "b.txt", Op: OpCreate},
		{Version: 3, Path: "a.txt", Op: OpUpdate},
		{Version: 4, Path: "b.txt", Op: OpDelete},
	}

	for _, e := range events {
		bc.Broadcast(e)
	}

	for i, expected := range events {
		select {
		case got := <-ch:
			if got.Version != expected.Version || got.Path != expected.Path || got.Op != expected.Op {
				t.Errorf("event %d: got (v%d %s %s), want (v%d %s %s)",
					i, got.Version, got.Path, got.Op,
					expected.Version, expected.Path, expected.Op)
			}
		case <-time.After(time.Second):
			t.Fatalf("timeout waiting for event %d", i)
		}
	}
}
