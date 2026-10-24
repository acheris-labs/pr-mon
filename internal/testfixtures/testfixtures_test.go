package testfixtures

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/acheris-labs/pr-mon/internal/twoway"
)

func TestTwoWayRecords(t *testing.T) {
	ctx := context.Background()
	fake := &TwoWay{}
	first, _ := fake.Post(ctx, twoway.Message{Title: "one"})
	second, _ := fake.Post(ctx, twoway.Message{Title: "two"})
	if first != "1" || second != "2" {
		t.Errorf("ids = %q, %q; want 1, 2", first, second)
	}
	fake.Edit(ctx, "1", twoway.Message{Title: "edited"})
	fake.Delete(ctx, "2")
	fake.Ack(ctx, twoway.Tap{Key: "k"})

	if len(fake.Posts) != 2 || fake.Edits["1"].Title != "edited" ||
		!reflect.DeepEqual(fake.Deletes, []string{"2"}) || !reflect.DeepEqual(fake.Acked, []twoway.Tap{{Key: "k"}}) {
		t.Errorf("recorded %+v", fake)
	}

	fake.PostErr, fake.DeleteErr = errors.New("post"), twoway.ErrTooOld
	if _, err := fake.Post(ctx, twoway.Message{}); err != fake.PostErr {
		t.Errorf("Post err = %v", err)
	}
	if err := fake.Delete(ctx, "1"); err != twoway.ErrTooOld {
		t.Errorf("Delete err = %v", err)
	}
	if len(fake.Posts) != 2 || len(fake.Deletes) != 2 {
		t.Errorf("after failures: %d posts, deletes %v", len(fake.Posts), fake.Deletes)
	}
}

func TestTwoWayReceiveWaitsForATap(t *testing.T) {
	fake := &TwoWay{Taps: make(chan twoway.Tap, 1)}
	fake.Taps <- twoway.Tap{MessageID: "1", Key: "k"}
	taps, err := fake.Receive(context.Background())
	if err != nil || !reflect.DeepEqual(taps, []twoway.Tap{{MessageID: "1", Key: "k"}}) {
		t.Errorf("Receive = %v, %v", taps, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	taps, err = fake.Receive(ctx)
	if taps != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Receive after the context ends = %v, %v", taps, err)
	}
}

func TestTwoWayUnavailable(t *testing.T) {
	ctx := context.Background()
	fake := &TwoWay{}
	fake.SetUnavailable(true)
	if _, err := fake.Post(ctx, twoway.Message{}); !errors.Is(err, twoway.ErrUnavailable) {
		t.Errorf("Post err = %v", err)
	}
	if err := fake.Edit(ctx, "1", twoway.Message{}); !errors.Is(err, twoway.ErrUnavailable) {
		t.Errorf("Edit err = %v", err)
	}
	if err := fake.Delete(ctx, "1"); !errors.Is(err, twoway.ErrUnavailable) {
		t.Errorf("Delete err = %v", err)
	}
	if len(fake.Posts) != 0 || len(fake.Edits) != 0 || len(fake.Deletes) != 0 {
		t.Errorf("recorded %+v while unavailable", fake)
	}
	fake.SetUnavailable(false)
	if id, err := fake.Post(ctx, twoway.Message{}); err != nil || id != "1" {
		t.Errorf("Post = %q, %v once available", id, err)
	}
}

func TestTwoWayClose(t *testing.T) {
	fake := &TwoWay{}
	if err := fake.Close(); err != nil || !fake.Closed {
		t.Errorf("Close = %v, Closed = %v", err, fake.Closed)
	}
}
