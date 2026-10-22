package home

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	log "github.com/sirupsen/logrus"
)

func TestClusterBalanceResponseErrors(t *testing.T) {
	for _, operation := range []string{"ENABLE", "RESULT", "CLAIM"} {
		for _, response := range []struct {
			name string
			raw  string
			fail bool
		}{
			{name: "malformed", raw: `{`, fail: true},
			{name: "wrong type", raw: `{"ok":"true"}`, fail: true},
			{name: "rejected", raw: `{"ok":false}`},
		} {
			t.Run(operation+"/"+response.name, func(t *testing.T) {
				client, _ := newRedisCommandTestClient(t, func(args []string) string {
					if strings.EqualFold(args[0], "CLUSTER") {
						return fmt.Sprintf("$%d\r\n%s\r\n", len(response.raw), response.raw)
					}
					return balanceTestRedisReply(args)
				})
				enableBalanceTestClient(client)
				var errResponse error
				switch operation {
				case "ENABLE":
					client.balance.registered = false
					errResponse = client.syncClusterBalance(context.Background())
				case "RESULT":
					client.balance.request.ID, client.balance.result = "pending", "failed"
					errResponse = client.syncClusterBalance(context.Background())
					if client.balance.request.ID != "pending" || client.balance.result != "failed" {
						t.Fatal("unacknowledged result was discarded")
					}
				case "CLAIM":
					errResponse = client.executeClusterRebalance(context.Background(), []byte(`{"id":"plan","term":1,"target":{"ip":"other","port":8327}}`))
				}
				if (errResponse != nil) != response.fail {
					t.Fatalf("response error = %v, want failure %v", errResponse, response.fail)
				}
				if response.fail && !strings.Contains(errResponse.Error(), operation+" response") {
					t.Fatalf("error lacks protocol context: %v", errResponse)
				}
				if client.balance.unsupported || client.balance.moving || (operation == "ENABLE" && client.balance.registered) {
					t.Fatal("invalid/rejected response advanced balance state or disabled capability")
				}
			})
		}
	}
}

func TestClusterBalanceRegistrationErrorsDoNotDisableCapability(t *testing.T) {
	client, _ := newRedisCommandTestClient(t, func(args []string) string {
		if strings.EqualFold(args[0], "CLUSTER") {
			return "-ERR membership_required\r\n"
		}
		return balanceTestRedisReply(args)
	})
	enableBalanceTestClient(client)
	client.balance.registered = false
	if errSync := client.syncClusterBalance(context.Background()); errSync == nil {
		t.Fatal("membership failure was swallowed")
	}
	if client.balance.unsupported || client.balance.registered {
		t.Fatal("membership failure disabled capability or registered client")
	}
}

func TestClusterBalanceBlockedRPCDoesNotDelayHeartbeatExpiry(t *testing.T) {
	for _, blockedOperation := range []string{"ENABLE", "CLAIM", "PING", "RESULT"} {
		t.Run(blockedOperation, func(t *testing.T) {
			blocked := make(chan struct{})
			release := make(chan struct{})
			heartbeats := newBlockedRPCTestHeartbeats()
			var blockOnce sync.Once
			block := func() {
				// The target RPC has entered the fixture: stop and join the simulated heartbeats
				// before signalling the test so that no frame can be written after the silence
				// starts. The subscriber's own 200ms window is left untouched.
				heartbeats.Stop()
				blockOnce.Do(func() { close(blocked) })
				<-release
			}
			target, _ := newRedisCommandTestClient(t, func(args []string) string {
				if blockedOperation == "PING" && strings.EqualFold(args[0], "PING") {
					block()
				}
				return balanceTestRedisReply(args)
			})
			instruction, errMarshal := json.Marshal(rebalanceInstruction{
				ID: "blocked-plan", Term: 1, Target: clusterNode{IP: target.homeCfg.Host, Port: target.homeCfg.Port},
			})
			if errMarshal != nil {
				t.Fatal(errMarshal)
			}
			client, commands := newBlockedRPCTestClient(t, heartbeats, func(args []string) string {
				switch strings.ToUpper(args[0]) {
				case "CLUSTER":
					if args[1] == "BALANCE" {
						if args[2] == blockedOperation {
							block()
						}
						return "$11\r\n{\"ok\":true}\r\n"
					}
					return "$22\r\n{\"ok\":true,\"nodes\":[]}\r\n"
				case "GET":
					return "$2\r\n{}\r\n"
				case "SUBSCRIBE":
					reply := "*3\r\n$9\r\nsubscribe\r\n$6\r\nconfig\r\n:1\r\n"
					if blockedOperation == "CLAIM" || blockedOperation == "PING" {
						reply += fmt.Sprintf("*3\r\n$7\r\nmessage\r\n$9\r\nrebalance\r\n$%d\r\n%s\r\n", len(instruction), instruction)
					}
					return reply
				default:
					return balanceTestRedisReply(args)
				}
			})
			t.Cleanup(func() { close(release) })
			enableBalanceTestClient(client)
			client.balance.registered = false
			client.testOperationTimeout = 5 * time.Second
			if blockedOperation == "RESULT" {
				client.balance.request.ID, client.balance.result = "previous-plan", "success"
			}
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			done := make(chan error, 1)
			go func() {
				done <- client.RunConfigSubscriberLifetime(ctx, func([]byte) error {
					return client.SetLifecycleConfig(config.CredentialConcurrencyConfig{
						LifecycleConfigRevision: 1, CPAHeartbeatTimeout: 200 * time.Millisecond,
					})
				}, nil)
			}()
			select {
			case <-blocked:
			case errRun := <-done:
				t.Fatalf("subscriber ended before blocked RPC: %v", errRun)
			case <-time.After(3 * time.Second):
				t.Fatal("RPC did not start")
			}
			select {
			case errRun := <-done:
				if !isTimeoutError(errRun) {
					t.Fatalf("subscriber error = %v, want heartbeat timeout", errRun)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("blocked RPC delayed heartbeat expiry or subscriber shutdown")
			}
			if client.HeartbeatOK() || !client.dispatchFenced.Load() {
				t.Fatal("heartbeat expiry did not fence dispatch admission")
			}
			if _, errDispatch := client.RPopAuth(context.Background(), "test-model", "", nil, 1); !errors.Is(errDispatch, ErrDispatchFenced) {
				t.Fatalf("late dispatch = %v, want fenced", errDispatch)
			}
			if len(findRedisCommand(commands.All(), "RPOP")) != 0 || client.balance.moving {
				t.Fatal("expired subscriber issued dispatch or migrated")
			}
			client.mu.Lock()
			commandPool := client.cmd
			client.mu.Unlock()
			if commandPool == nil {
				t.Fatal("migration cancellation detached the source command pool")
			}
			if errPing := commandPool.Ping(context.Background()).Err(); errPing != nil {
				t.Fatalf("migration cancellation interrupted the source command pool: %v", errPing)
			}
		})
	}
}

// blockedRPCTestHeartbeatInterval and blockedRPCTestHeartbeatFrame simulate the periodic Home
// heartbeats that refresh the subscriber's read deadline. The channel is unknown to the client,
// so a heartbeat is consumed without touching the lifecycle config.
const (
	blockedRPCTestHeartbeatInterval = 25 * time.Millisecond
	blockedRPCTestHeartbeatFrame    = "*3\r\n$7\r\nmessage\r\n$14\r\nhome-heartbeat\r\n$1\r\n1\r\n"
)

// blockedRPCTestHeartbeats owns the simulated heartbeat producers of one fixture connection
// set. Stop closes the silence window and joins every producer, so after it returns no frame can
// be written any more.
type blockedRPCTestHeartbeats struct {
	mu      sync.Mutex
	silence chan struct{}
	stopped bool
	wg      sync.WaitGroup
}

func newBlockedRPCTestHeartbeats() *blockedRPCTestHeartbeats {
	return &blockedRPCTestHeartbeats{silence: make(chan struct{})}
}

// Stop ends the silence window and waits for all producers to exit. It is safe to call more than
// once and also safe when no producer ever started (for example when a command is blocked before
// the subscription is established).
func (h *blockedRPCTestHeartbeats) Stop() {
	if h == nil {
		return
	}
	h.mu.Lock()
	if !h.stopped {
		h.stopped = true
		close(h.silence)
	}
	h.mu.Unlock()
	h.wg.Wait()
}

// Start launches a producer that pushes heartbeat frames on conn until Stop is called or the
// connection fails.
func (h *blockedRPCTestHeartbeats) Start(conn net.Conn, writeMu *sync.Mutex) {
	if h == nil {
		return
	}
	h.mu.Lock()
	if h.stopped {
		h.mu.Unlock()
		return
	}
	h.wg.Add(1)
	h.mu.Unlock()
	go func() {
		defer h.wg.Done()
		ticker := time.NewTicker(blockedRPCTestHeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-h.silence:
				return
			case <-ticker.C:
				writeMu.Lock()
				_, errWrite := io.WriteString(conn, blockedRPCTestHeartbeatFrame)
				writeMu.Unlock()
				if errWrite != nil {
					return
				}
			}
		}
	}()
}

// newBlockedRPCTestClient mirrors newRedisCommandTestClient and additionally keeps the
// subscriber's heartbeat window alive with simulated Home pushes. The subscriber therefore stays
// alive while it is still dispatching setup work, and a test starts the silence window exactly
// when it blocks an RPC instead of racing that window against an unbounded scheduling delay.
func newBlockedRPCTestClient(t *testing.T, heartbeats *blockedRPCTestHeartbeats, handler func([]string) string) (*Client, *redisCommandLog) {
	t.Helper()
	listener, errListen := net.Listen("tcp", "127.0.0.1:0")
	if errListen != nil {
		t.Fatalf("listen: %v", errListen)
	}
	commands := &redisCommandLog{}
	var connections sync.WaitGroup
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		for {
			conn, errAccept := listener.Accept()
			if errAccept != nil {
				return
			}
			connections.Add(1)
			go func(conn net.Conn) {
				defer connections.Done()
				defer func() { _ = conn.Close() }()
				serveBlockedRPCTestConn(conn, commands, handler, heartbeats)
			}(conn)
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		<-serverDone
		// Blocked RPCs are released by a cleanup registered after this fixture, so handlers can
		// exit and be joined. Heartbeats stop first so their producers cannot outlive the conns.
		heartbeats.Stop()
		connections.Wait()
	})
	addr, okAddr := listener.Addr().(*net.TCPAddr)
	if !okAddr {
		t.Fatalf("unexpected listener address %v", listener.Addr())
	}
	client := New(config.HomeConfig{
		Enabled: true, Host: addr.IP.String(), Port: addr.Port, DisableClusterDiscovery: true,
	})
	client.testOperationTimeout = homeRedisTestOperationTimeout
	t.Cleanup(func() { client.Close() })
	return client, commands
}

func serveBlockedRPCTestConn(conn net.Conn, commands *redisCommandLog, handler func([]string) string, heartbeats *blockedRPCTestHeartbeats) {
	reader := bufio.NewReader(conn)
	var writeMu sync.Mutex
	for {
		args, errRead := readRedisCommand(reader)
		if errRead != nil {
			return
		}
		commands.Append(args)
		reply := "+OK\r\n"
		if handler != nil {
			reply = handler(args)
		}
		writeMu.Lock()
		_, errWrite := io.WriteString(conn, reply)
		writeMu.Unlock()
		if errWrite != nil {
			return
		}
		if strings.EqualFold(args[0], "SUBSCRIBE") {
			heartbeats.Start(conn, &writeMu)
		}
	}
}

func TestClusterBalanceMigrationLogIncludesPlanAndAddresses(t *testing.T) {
	var output bytes.Buffer
	logger := log.StandardLogger()
	previousOutput, previousLevel := logger.Out, logger.GetLevel()
	logger.SetOutput(&output)
	logger.SetLevel(log.InfoLevel)
	t.Cleanup(func() {
		logger.SetOutput(previousOutput)
		logger.SetLevel(previousLevel)
	})
	target, _ := newRedisCommandTestClient(t, balanceTestRedisReply)
	client, _ := newRedisCommandTestClient(t, func(args []string) string {
		if strings.EqualFold(args[0], "CLUSTER") {
			return "$11\r\n{\"ok\":true}\r\n"
		}
		return balanceTestRedisReply(args)
	})
	enableBalanceTestClient(client)
	origin := clusterNodeAddress(clusterNode{IP: client.homeCfg.Host, Port: client.homeCfg.Port})
	request := rebalanceInstruction{ID: "logged-plan", Term: 1, Target: clusterNode{IP: target.homeCfg.Host, Port: target.homeCfg.Port}}
	payload, errMarshal := json.Marshal(request)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	if errExecute := client.executeClusterRebalance(context.Background(), payload); !errors.Is(errExecute, ErrRebalanceRequested) {
		t.Fatal(errExecute)
	}
	// The message itself must carry these values, independently of formatter field allowlists.
	line := strings.ReplaceAll(output.String(), `\"`, `"`)
	for key, value := range map[string]string{"plan": request.ID, "from": origin, "to": clusterNodeAddress(request.Target)} {
		if !strings.Contains(line, fmt.Sprintf("%s=%q", key, value)) {
			t.Fatalf("migration log missing %s: %q", key, line)
		}
	}
}

func TestClusterBalanceMalformedRegistrationEndsSubscriberBeforeReady(t *testing.T) {
	client, _ := newRedisCommandTestClient(t, func(args []string) string {
		switch strings.ToUpper(args[0]) {
		case "CLUSTER":
			if args[1] == "BALANCE" {
				return "$1\r\n{\r\n"
			}
			return "$22\r\n{\"ok\":true,\"nodes\":[]}\r\n"
		case "GET":
			return "$2\r\n{}\r\n"
		case "SUBSCRIBE":
			return "*3\r\n$9\r\nsubscribe\r\n$6\r\nconfig\r\n:1\r\n"
		default:
			return balanceTestRedisReply(args)
		}
	})
	enableBalanceTestClient(client)
	client.balance.registered = false
	ready := false
	errRun := client.RunConfigSubscriberLifetime(context.Background(), func([]byte) error { return nil }, func() { ready = true })
	var syntaxError *json.SyntaxError
	if !errors.As(errRun, &syntaxError) || !strings.Contains(errRun.Error(), "ENABLE response") {
		t.Fatalf("subscriber did not propagate decoding failure: %v", errRun)
	}
	if ready || client.HeartbeatOK() {
		t.Fatal("malformed registration published a healthy subscriber")
	}
}
