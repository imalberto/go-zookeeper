package zk

import (
	"log"
	"sync"
	"testing"
	"time"
)

type logWriter struct {
	t *testing.T
	p string
}

func (lw logWriter) Write(b []byte) (int, error) {
	lw.t.Logf("%s%s", lw.p, string(b))
	return len(b), nil
}

func TestBasicCluster(t *testing.T) {
	ts, err := StartTestCluster(3, nil, logWriter{t: t, p: "[ZKERR] "})
	if err != nil {
		t.Fatal(err)
	}
	defer ts.Stop()
	zk1, err := ts.Connect(0)
	if err != nil {
		t.Fatalf("Connect returned error: %+v", err)
	}
	defer zk1.Close()
	zk2, err := ts.Connect(1)
	if err != nil {
		t.Fatalf("Connect returned error: %+v", err)
	}
	defer zk2.Close()

	time.Sleep(time.Second * 5)

	if _, err := zk1.Create("/gozk-test", []byte("foo-cluster"), 0, WorldACL(PermAll)); err != nil {
		t.Fatalf("Create failed on node 1: %+v", err)
	}
	if by, _, err := zk2.Get("/gozk-test"); err != nil {
		t.Fatalf("Get failed on node 2: %+v", err)
	} else if string(by) != "foo-cluster" {
		t.Fatal("Wrong data for node 2")
	}
}

// If the current leader dies, then the session is reestablished with the new one.
func TestClientClusterFailover(t *testing.T) {
	tc, err := StartTestCluster(3, nil, logWriter{t: t, p: "[ZKERR] "})
	if err != nil {
		t.Fatal(err)
	}
	defer tc.Stop()
	zk, evCh, err := tc.ConnectAll()
	if err != nil {
		t.Fatalf("Connect returned error: %+v", err)
	}
	defer zk.Close()

	sl := NewStateLogger(evCh)

	hasSessionEvent1 := sl.NewWatcher(sessionStateMatcher(StateHasSession)).Wait(8 * time.Second)
	if hasSessionEvent1 == nil {
		t.Fatalf("Failed to connect and get session")
	}

	if _, err := zk.Create("/gozk-test", []byte("foo-cluster"), 0, WorldACL(PermAll)); err != nil {
		t.Fatalf("Create failed on node 1: %+v", err)
	}

	hasSessionWatcher2 := sl.NewWatcher(sessionStateMatcher(StateHasSession))

	// Kill the current leader
	tc.StopServer(hasSessionEvent1.Server)

	// Wait for the session to be reconnected with the new leader.
	hasSessionWatcher2.Wait(8 * time.Second)
	if hasSessionWatcher2 == nil {
		t.Fatalf("Failover failed")
	}

	if by, _, err := zk.Get("/gozk-test"); err != nil {
		t.Fatalf("Get failed on node 2: %+v", err)
	} else if string(by) != "foo-cluster" {
		t.Fatal("Wrong data for node 2")
	}
}

// If a ZooKeeper cluster looses quorum then a session is reconnected as soon
// as the quorum is restored.
func TestNoQuorum(t *testing.T) {
	tc, err := StartTestCluster(3, nil, logWriter{t: t, p: "[ZKERR] "})
	if err != nil {
		t.Fatal(err)
	}
	defer tc.Stop()
	zk, evCh, err := tc.ConnectAllTimeout(4 * time.Second)
	if err != nil {
		t.Fatalf("Connect returned error: %+v", err)
	}
	defer zk.Close()
	sl := NewStateLogger(evCh)

	// Wait for initial session to be established
	hasSessionEvent1 := sl.NewWatcher(sessionStateMatcher(StateHasSession)).Wait(8 * time.Second)
	if hasSessionEvent1 == nil {
		t.Fatalf("Failed to connect and get session")
	}
	initialSessionID := zk.sessionID
	DefaultLogger.Printf("    Session established: id=%d, timeout=%d", zk.sessionID, zk.sessionTimeoutMs)

	// Kill the ZooKeeper leader and wait for the session to reconnect.
	DefaultLogger.Printf("    Kill the leader")
	hasSessionWatcher2 := sl.NewWatcher(sessionStateMatcher(StateHasSession))
	tc.StopServer(hasSessionEvent1.Server)
	hasSessionEvent2 := hasSessionWatcher2.Wait(8 * time.Second)
	if hasSessionEvent2 == nil {
		t.Fatalf("Failover failed")
	}

	// Kill the ZooKeeper leader leaving the cluster without quorum.
	DefaultLogger.Printf("    Kill the leader")
	// Ensure that the first StateDisconnected event that is captured is
	// from this session
	// go func() {
	// 	tc.StopServer(hasSessionEvent2.Server)
	// }()
	tc.StopServer(hasSessionEvent2.Server)
	tmpEvent := sl.NewWatcher(sessionStateMatcher(StateDisconnected)).Wait(4 * time.Second)
	if tmpEvent == nil {
		t.Fatalf("StateDisconnected event not received from server: %s", hasSessionEvent2.Server)
	}

	// Make sure that we keep retrying connecting to the only remaining
	// ZooKeeper server, but the attempts are being dropped because there is
	// no quorum.
	DefaultLogger.Printf("    ==================================")
	DefaultLogger.Printf("    Retrying no luck...")
	var firstDisconnect *Event
	begin := time.Now()
	for time.Now().Sub(begin) < 6*time.Second {
		disconnectedEvent := sl.NewWatcher(sessionStateMatcher(StateDisconnected)).Wait(4 * time.Second)
		if disconnectedEvent == nil {
			t.Fatalf("Disconnected event expected")
		}
		if firstDisconnect == nil {
			firstDisconnect = disconnectedEvent
			continue
		}
		if disconnectedEvent.Server != firstDisconnect.Server {
			t.Fatalf("Disconnect from wrong server: expected=%s, actual=%s",
				firstDisconnect.Server, disconnectedEvent.Server)
		}
		// if hasSessionEvent2.Server != firstDisconnect.Server {
		// 	t.Fatalf("Disconnect from wrong server: expected=%s, actual=%s",
		// 		firstDisconnect.Server, hasSessionEvent2.Server)
		// }
	}

	// Start a ZooKeeper node to restore quorum.
	hasSessionWatcher3 := sl.NewWatcher(sessionStateMatcher(StateHasSession))
	tc.StartServer(hasSessionEvent1.Server)

	// Make sure that session is reconnected with the same ID.
	hasSessionEvent3 := hasSessionWatcher3.Wait(8 * time.Second)
	if hasSessionEvent3 == nil {
		t.Fatalf("Session has not been reconnected")
	}
	if zk.sessionID != initialSessionID {
		t.Fatalf("Wrong session ID: expected=%d, actual=%d", initialSessionID, zk.sessionID)
	}

	// Make sure that the session is not dropped soon after reconnect
	e := sl.NewWatcher(sessionStateMatcher(StateDisconnected)).Wait(6 * time.Second)
	if e != nil {
		t.Fatalf("Unexpected disconnect")
	}
}

func TestWaitForClose(t *testing.T) {
	ts, err := StartTestCluster(1, nil, logWriter{t: t, p: "[ZKERR] "})
	if err != nil {
		t.Fatal(err)
	}
	defer ts.Stop()
	zk, err := ts.Connect(0)
	if err != nil {
		t.Fatalf("Connect returned error: %+v", err)
	}
	timeout := time.After(30 * time.Second)
CONNECTED:
	for {
		select {
		case ev := <-zk.eventChan:
			if ev.State == StateConnected {
				break CONNECTED
			}
		case <-timeout:
			zk.Close()
			t.Fatal("Timeout")
		}
	}
	zk.Close()
	for {
		select {
		case _, ok := <-zk.eventChan:
			if !ok {
				return
			}
		case <-timeout:
			t.Fatal("Timeout")
		}
	}
}

func TestBadSession(t *testing.T) {
	ts, err := StartTestCluster(1, nil, logWriter{t: t, p: "[ZKERR] "})
	if err != nil {
		t.Fatal(err)
	}
	defer ts.Stop()
	zk, _, err := ts.ConnectAll()
	if err != nil {
		t.Fatalf("Connect returned error: %+v", err)
	}
	defer zk.Close()

	if err := zk.Delete("/gozk-test", -1); err != nil && err != ErrNoNode {
		t.Fatalf("Delete returned error: %+v", err)
	}

	zk.conn.Close()
	time.Sleep(time.Millisecond * 100)

	if err := zk.Delete("/gozk-test", -1); err != nil && err != ErrNoNode {
		t.Fatalf("Delete returned error: %+v", err)
	}
}

type EventLogger struct {
	events   []Event
	watchers []*EventWatcher
	lock     sync.Mutex
	wg       sync.WaitGroup
}

func NewStateLogger(eventCh <-chan Event) *EventLogger {
	el := &EventLogger{}
	el.wg.Add(1)
	go func() {
		defer el.wg.Done()
		for event := range eventCh {
			el.lock.Lock()
			// For each event, let's print the list and order of the watchers
			for _, sw := range el.watchers {
				if !sw.triggered && sw.matcher(event) {
					log.Printf("      [%s] %s eventWatcher: %+v <<<<<<", event.Server, event.State, sw)
					sw.triggered = true
					sw.matchCh <- event
					// } else {
					// log.Printf("      [%s] %s eventWatcher: %+v", event.Server, event.State, sw)
				}
			}
			DefaultLogger.Printf("    event received: %v\n", event)
			el.events = append(el.events, event)
			el.lock.Unlock()
		}
	}()
	return el
}

func (el *EventLogger) NewWatcher(matcher func(Event) bool) *EventWatcher {
	ew := &EventWatcher{matcher: matcher, matchCh: make(chan Event, 1)}
	el.lock.Lock()
	el.watchers = append(el.watchers, ew)
	el.lock.Unlock()
	return ew
}

func (el *EventLogger) Events() []Event {
	el.lock.Lock()
	transitions := make([]Event, len(el.events))
	copy(transitions, el.events)
	el.lock.Unlock()
	return transitions
}

func (el *EventLogger) Wait4Stop() {
	el.wg.Wait()
}

type EventWatcher struct {
	matcher   func(Event) bool
	matchCh   chan Event
	triggered bool
}

func (ew *EventWatcher) Wait(timeout time.Duration) *Event {
	select {
	case event := <-ew.matchCh:
		return &event
	case <-time.After(timeout):
		return nil
	}
}

func sessionStateMatcher(s State) func(Event) bool {
	return func(e Event) bool {
		return e.Type == EventSession && e.State == s
	}
}
