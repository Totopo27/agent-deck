package main

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRemoteAgentProfileResolution verifies that remote-agent honors
// AGENTDECK_PROFILE and default_profile in config when launched without -p (#2755).
func TestRemoteAgentProfileResolution(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess CLI test skipped in short mode")
	}

	home := t.TempDir()

	// 1. Configure default_profile=work in config.json
	configDir := filepath.Join(home, ".config", "agent-deck")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("mkdir config: %v", err)
	}
	configContent := `{"default_profile": "work"}`
	if err := os.WriteFile(filepath.Join(configDir, "config.json"), []byte(configContent), 0o644); err != nil {
		t.Fatalf("write config.json: %v", err)
	}

	// Create profile "work" and add an initial session to it
	projDir := filepath.Join(home, "proj")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("mkdir proj: %v", err)
	}

	// AGENTDECK_PROFILE="" ensures fallback to default_profile ("work")
	env := []string{"AGENTDECK_PROFILE="}

	stdout, stderr, code := runAgentDeckEnv(t, home, "", env, "profile", "create", "work")
	if code != 0 {
		t.Fatalf("profile create work failed (%d):\n%s\n%s", code, stdout, stderr)
	}

	stdout, stderr, code = runAgentDeckEnv(t, home, "", env, "add", projDir, "-t", "one", "-c", "shell", "--json")
	if code != 0 {
		t.Fatalf("add session one failed (%d):\n%s\n%s", code, stdout, stderr)
	}

	// Verify work's state.db exists and default's state.db does NOT
	workDBPath := filepath.Join(home, ".local", "share", "agent-deck", "profiles", "work", "state.db")
	if _, err := os.Stat(workDBPath); err != nil {
		t.Fatalf("work state.db was not created: %v", err)
	}
	defaultDBPath := filepath.Join(home, ".local", "share", "agent-deck", "state.db")
	if _, err := os.Stat(defaultDBPath); err == nil {
		t.Fatalf("default state.db should not exist")
	}

	// Start `agent-deck remote-agent` without -p
	cmd := exec.Command(channelsCLIBinary(t), "remote-agent")
	cmd.Env = agentDeckTestEnv(home, env)

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("start remote-agent: %v", err)
	}
	t.Cleanup(func() {
		_ = stdinPipe.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	scanner := bufio.NewScanner(stdoutPipe)

	readReply := func(timeout time.Duration) (remoteAgentReply, error) {
		type res struct {
			r   remoteAgentReply
			err error
		}
		ch := make(chan res, 1)
		go func() {
			if scanner.Scan() {
				var r remoteAgentReply
				err := json.Unmarshal(scanner.Bytes(), &r)
				ch <- res{r: r, err: err}
			} else {
				err := scanner.Err()
				if err == nil {
					err = io.EOF
				}
				ch <- res{err: err}
			}
		}()
		select {
		case out := <-ch:
			return out.r, out.err
		case <-time.After(timeout):
			t.Fatalf("timed out waiting for reply from remote-agent")
			return remoteAgentReply{}, nil
		}
	}

	// First event should be "ready"
	reply, err := readReply(5 * time.Second)
	if err != nil {
		t.Fatalf("reading ready event: %v", err)
	}
	if reply.Event != "ready" {
		t.Fatalf("expected ready event, got: %+v", reply)
	}

	// Test request runner uses the resolved profile: request a list
	req := remoteAgentRequest{
		ID:   1,
		Args: []string{"list", "--json"},
	}
	reqBytes, _ := json.Marshal(req)
	if _, err := stdinPipe.Write(append(reqBytes, '\n')); err != nil {
		t.Fatalf("writing request: %v", err)
	}

	reply, err = readReply(5 * time.Second)
	if err != nil {
		t.Fatalf("reading list reply: %v", err)
	}
	if reply.ID != 1 || reply.Code != 0 {
		t.Fatalf("expected successful reply for ID 1, got: %+v", reply)
	}
	if !strings.Contains(reply.Stdout, "one") {
		t.Fatalf("list output should contain session 'one', got: %s", reply.Stdout)
	}

	// Now add a second session "two" to the work profile
	stdout, stderr, code = runAgentDeckEnv(t, home, "", env, "add", projDir, "-t", "two", "-c", "shell", "--json")
	if code != 0 {
		t.Fatalf("add session two failed (%d):\n%s\n%s", code, stdout, stderr)
	}

	// The agent should detect the change in work's state.db and push a "changed" event
	changedReply, err := readReply(10 * time.Second)
	if err != nil {
		t.Fatalf("reading changed event: %v", err)
	}
	if changedReply.Event != "changed" {
		t.Fatalf("expected 'changed' event, got: %+v", changedReply)
	}
	if !strings.Contains(changedReply.Sessions, "two") {
		t.Fatalf("changed event sessions should contain 'two', got: %s", changedReply.Sessions)
	}
}
