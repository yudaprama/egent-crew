package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"
)

func findTool(t *testing.T, ts []tool.InvokableTool, name string) tool.InvokableTool {
	t.Helper()
	for _, tt := range ts {
		if info, err := tt.Info(context.Background()); err == nil && info.Name == name {
			return tt
		}
	}
	t.Fatalf("tool %q not found", name)
	return nil
}

func TestRunCommand_Sync(t *testing.T) {
	ts, err := buildShell(context.Background())
	if err != nil {
		t.Fatalf("buildShell: %v", err)
	}
	run := findTool(t, ts, "run_command")

	out, err := run.InvokableRun(context.Background(), `{"command":"echo hello-crew && true"}`)
	if err != nil {
		t.Fatalf("InvokableRun: %v", err)
	}
	var res struct {
		Success  bool   `json:"success"`
		ExitCode int    `json:"exit_code"`
		Output   string `json:"output"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("unmarshal %s: %v", out, err)
	}
	if !res.Success || res.ExitCode != 0 {
		t.Fatalf("expected success/exit 0, got %+v", res)
	}
	if !strings.Contains(res.Output, "hello-crew") {
		t.Fatalf("output missing marker: %+v", res)
	}
}

func TestRunCommand_TimeoutEnforced(t *testing.T) {
	ts, _ := buildShell(context.Background())
	run := findTool(t, ts, "run_command")

	start := time.Now()
	out, _ := run.InvokableRun(context.Background(), `{"command":"sleep 10","timeout":1}`)
	elapsed := time.Since(start)

	var res struct {
		Success bool `json:"success"`
	}
	_ = json.Unmarshal([]byte(out), &res)

	if elapsed > 5*time.Second {
		t.Fatalf("timeout not enforced: elapsed=%v", elapsed)
	}
	if res.Success {
		t.Fatalf("sleep should not have succeeded within timeout; got %s", out)
	}
}

func TestRunCommand_BackgroundAndKill(t *testing.T) {
	ts, _ := buildShell(context.Background())
	run := findTool(t, ts, "run_command")
	kill := findTool(t, ts, "kill_command")

	out, err := run.InvokableRun(context.Background(), `{"command":"sleep 30","run_in_background":true}`)
	if err != nil {
		t.Fatalf("background start: %v", err)
	}
	var started struct {
		Success bool   `json:"success"`
		ShellID string `json:"shell_id"`
	}
	if err := json.Unmarshal([]byte(out), &started); err != nil {
		t.Fatalf("unmarshal %s: %v", out, err)
	}
	if !started.Success || started.ShellID == "" {
		t.Fatalf("expected background shell_id, got %s", out)
	}

	kout, err := kill.InvokableRun(context.Background(), `{"shell_id":"`+started.ShellID+`"}`)
	if err != nil {
		t.Fatalf("kill: %v", err)
	}
	var killed struct {
		Success bool `json:"success"`
	}
	_ = json.Unmarshal([]byte(kout), &killed)
	if !killed.Success {
		t.Fatalf("kill failed: %s", kout)
	}
}
