package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"runtime"
	"sync"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/yudaprama/tools/localfs"
)

const defaultCommandTimeout = 120 // seconds

type runCommandInput struct {
	Command         string `json:"command" jsonschema:"required,description=Shell command string. Runs via sh -c so pipes (|), chaining (&&), redirection (>), env vars ($X), backticks, and globs all work. Executes in the workspace cwd."`
	RunInBackground bool   `json:"run_in_background,omitempty" jsonschema:"description=If true, start detached and return a shell_id (stop it later with kill_command). If false (default), run synchronously and return combined stdout+stderr plus exit_code."`
	Timeout         int    `json:"timeout,omitempty" jsonschema:"description=Max seconds before the command is killed. Default 120. Honored even for synchronous commands."`
}

// local background shell tracking (used when localfs doesn't enforce timeouts)
var (
	bgCmds   = make(map[string]*exec.Cmd)
	bgCmdsMu sync.Mutex
)

type killCommandInput struct {
	ShellID string `json:"shell_id" jsonschema:"required,description=The shell_id returned by run_command when run_in_background was true"`
}

// buildShell exposes run_command + kill_command backed by tools/localfs.Service.
//
// This is the host-native shell tool (sh -c) that fills the gap eino-ext leaves
// (Operator.RunCommand is internal, not agent-callable). It runs in the process
// cwd, which main.go sets to $CREW_WORKSPACE at startup so relative paths and
// git/build commands resolve against the project root. Timeout is enforced here
// via context.WithTimeout — the Service does not apply the Timeout field itself.
//
// SECURITY: this is a convenience scope, NOT a security sandbox. The command
// runs with the full privileges of the egent-crew process user and can touch
// anything on the host. Use eino-ext `commandline` (Docker) instead when you
// need isolation for untrusted code.
func buildShell(_ context.Context) ([]tool.InvokableTool, error) {
	svc := localfs.NewService()

	runTool, err := utils.InferTool("run_command",
		"Run a shell command in the workspace (sh -c). Supports pipes, &&, redirects, env vars, globs. "+
			"Use for build, test, git, deploy, lint, and any dev-shell task. Returns JSON with success, "+
			"exit_code, and output (combined stdout+stderr). For long-running processes (dev servers, "+
			"`docker compose up`) set run_in_background=true and stop later with kill_command.",
		func(ctx context.Context, in *runCommandInput) (string, error) {
			if in.Command == "" {
				return "", fmt.Errorf("command is required")
			}
			timeout := in.Timeout
			if timeout <= 0 {
				timeout = defaultCommandTimeout
			}

			if in.RunInBackground {
				res, runErr := svc.RunCommand(ctx, localfs.RunCommandParams{
					Command:         in.Command,
					RunInBackground: true,
					Timeout:         timeout,
				})
				if res != nil {
					out, _ := json.Marshal(res)
					return string(out), runErr
				}
				return "", runErr
			}

			// Synchronous: enforce timeout via context + manual kill
			cctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
			defer cancel()

			cmd := makeShellCmd(cctx, in.Command)
			var mu sync.Mutex
			killed := false

			go func() {
				<-cctx.Done()
				mu.Lock()
				defer mu.Unlock()
				if cmd.Process != nil && cctx.Err() == context.DeadlineExceeded {
					killed = true
					cmd.Process.Kill()
				}
			}()

			output, err := cmd.CombinedOutput()
			exitCode := 0
			if err != nil {
				if exitErr, ok := err.(*exec.ExitError); ok {
					exitCode = exitErr.ExitCode()
				}
			}

			mu.Lock()
			wasKilled := killed
			mu.Unlock()

			if wasKilled {
				return fmt.Sprintf(`{"success":false,"exit_code":-1,"output":"command killed after %ds timeout","stdout":"","stderr":"%s"}`,
					timeout, escapeJSON(string(output))), nil
			}

			return fmt.Sprintf(`{"success":%v,"exit_code":%d,"output":%s,"stdout":%s,"stderr":""}`,
				err == nil, exitCode, escapeJSON(string(output)), escapeJSON(string(output))), nil
		},
	)
	if err != nil {
		return nil, fmt.Errorf("infer run_command: %w", err)
	}

	killTool, err := utils.InferTool("kill_command",
		"Stop a background command started by run_command (run_in_background=true), identified by its shell_id.",
		func(ctx context.Context, in *killCommandInput) (string, error) {
			if in.ShellID == "" {
				return "", fmt.Errorf("shell_id is required")
			}
			res, err := svc.KillCommand(ctx, localfs.KillCommandParams{ShellID: in.ShellID})
			if res != nil {
				out, _ := json.Marshal(res)
				return string(out), err
			}
			return "", err
		},
	)
	if err != nil {
		return nil, fmt.Errorf("infer kill_command: %w", err)
	}

	return []tool.InvokableTool{runTool, killTool}, nil
}

func makeShellCmd(ctx context.Context, command string) *exec.Cmd {
	switch runtime.GOOS {
	case "windows":
		return exec.CommandContext(ctx, "cmd", "/C", command)
	default:
		return exec.CommandContext(ctx, "sh", "-c", command)
	}
}

func escapeJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
