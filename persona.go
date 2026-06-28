package main

import (
	"context"
	"embed"
	"fmt"
	"log"
	"path"
	"strings"

	"github.com/cloudwego/eino-ext/components/tool/browseruse"
	"github.com/cloudwego/eino-ext/components/tool/commandline"
	"github.com/cloudwego/eino-ext/components/tool/commandline/sandbox"
	"github.com/cloudwego/eino-ext/components/tool/httprequest"
	"github.com/cloudwego/eino/components/tool"
	"github.com/yudaprama/tools"
	"github.com/yudaprama/tools/builtin"
	"gopkg.in/yaml.v3"
)

// Persona is one role served by egent-crew. Its ID matches the Plano agent id
// (the value Plano stamps in the x-arch-upstream header), so a request can be
// dispatched to the right runner with no extra mapping.
type Persona struct {
	ID           string   `yaml:"id"`
	Default      bool     `yaml:"default"`
	SystemPrompt string   `yaml:"system_prompt"`
	Tools        []string `yaml:"tools"`
}

// loadPersonas reads every personas/*.yaml under root (an embedded FS). At
// least one persona must carry default: true so the dispatcher always has a
// fallback when x-arch-upstream is absent or unknown.
func loadPersonas(root string, embedded embed.FS) ([]Persona, error) {
	entries, err := embedded.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read personas dir %q: %w", root, err)
	}

	var personas []Persona
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		raw, err := embedded.ReadFile(path.Join(root, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", e.Name(), err)
		}
		var p Persona
		if err := yaml.Unmarshal(raw, &p); err != nil {
			return nil, fmt.Errorf("parse %s: %w", e.Name(), err)
		}
		if p.ID == "" {
			return nil, fmt.Errorf("%s: persona id is required", e.Name())
		}
		personas = append(personas, p)
		log.Printf("loaded persona %q (%d tools, default=%v)", p.ID, len(p.Tools), p.Default)
	}
	if len(personas) == 0 {
		return nil, fmt.Errorf("no personas found under %s", root)
	}
	return personas, nil
}

// defaultPersona returns the persona flagged default:true, or the first one if
// none is flagged (mirrors Plano's own fallback semantics).
func defaultPersona(personas []Persona) Persona {
	for _, p := range personas {
		if p.Default {
			return p
		}
	}
	return personas[0]
}

// groupBuilders maps a logical group name to its constructor. Only the
// groups actually referenced by some persona are built. Builtin groups come
// from tools/builtin (host execution); the eino-ext groups are sandboxed /
// heavyweight (Docker for commandline, Chrome for browseruse) and are built
// best-effort — see buildRegistry.
var groupBuilders = map[string]func(context.Context) ([]tool.InvokableTool, error){
	// tools/builtin — host-native
	"code-interpreter":  builtin.NewCodeInterpreter,
	"office-word":       builtin.NewOfficeWord,
	"office-excel":      builtin.NewOfficeExcel,
	"office-powerpoint": builtin.NewOfficePowerPoint,
	"calculator":        builtin.NewCalculator,
	"pdf":               builtin.NewPDF,
	"image-designer":    builtin.NewImageDesigner,
	"web-browsing":      builtin.NewWebBrowsing,
	"local-system":      builtin.NewLocalSystem,
	// eino-ext — sandboxed / external-runtime
	"httprequest": buildHTTPRequest,
	"browseruse":  buildBrowserUse,
	"commandline": buildCommandLine,
	// host shell — tools/localfs.RunCommand wrapped as InvokableTool (see shell.go)
	"shell": buildShell,
}

// buildHTTPRequest mounts the generic REST tools (request_get / requests_post /
// requests_put / requests_delete). Lightweight — always available.
func buildHTTPRequest(ctx context.Context) ([]tool.InvokableTool, error) {
	ts, err := httprequest.NewToolKit(ctx, &httprequest.Config{})
	if err != nil {
		return nil, err
	}
	out := make([]tool.InvokableTool, 0, len(ts))
	for _, t := range ts {
		if it, ok := t.(tool.InvokableTool); ok {
			out = append(out, it)
		}
	}
	return out, nil
}

// buildBrowserUse mounts the `browser_use` tool (Playwright-style automation
// via chromedp). Construction is lazy — Chrome is only needed at run time, so
// this mounts even without a browser installed (calls then error back to the LLM).
func buildBrowserUse(ctx context.Context) ([]tool.InvokableTool, error) {
	t, err := browseruse.NewBrowserUseTool(ctx, &browseruse.Config{Headless: true})
	if err != nil {
		return nil, err
	}
	return []tool.InvokableTool{t}, nil
}

// buildCommandLine mounts `python_execute` + `str_replace_editor` on a Docker
// sandbox. Requires the Docker daemon; if unavailable the group is skipped
// (best-effort in buildRegistry) and personas referencing it get a curate() warning.
// Note: this exposes ONLY the editor + python tools — there is no shell/bash
// tool in eino-ext (Operator.RunCommand is an internal interface method).
func buildCommandLine(ctx context.Context) ([]tool.InvokableTool, error) {
	sb, err := sandbox.NewDockerSandbox(ctx, &sandbox.Config{})
	if err != nil {
		return nil, fmt.Errorf("docker sandbox: %w", err)
	}
	if err := sb.Create(ctx); err != nil {
		return nil, fmt.Errorf("docker sandbox create: %w", err)
	}
	py, err := commandline.NewPyExecutor(ctx, &commandline.PyExecutorConfig{Operator: sb})
	if err != nil {
		return nil, err
	}
	ed, err := commandline.NewStrReplaceEditor(ctx, &commandline.EditorConfig{Operator: sb})
	if err != nil {
		return nil, err
	}
	return []tool.InvokableTool{py, ed}, nil
}

// groupForTool resolves the group that owns a tool name. Returns "" for names
// this binary does not serve.
func groupForTool(name string) string {
	switch {
	// eino-ext (exact names — note the request_/requests_ inconsistency upstream)
	case name == "request_get", name == "requests_post", name == "requests_put", name == "requests_delete":
		return "httprequest"
	case name == "browser_use":
		return "browseruse"
	case name == "python_execute", name == "str_replace_editor":
		return "commandline"
	// host shell (tools/localfs-backed, exposed in shell.go)
	case name == "run_command", name == "kill_command":
		return "shell"
	// tools/builtin (prefix match)
	case name == "calculator":
		return "calculator"
	case strings.HasPrefix(name, "lobe-code-interpreter"):
		return "code-interpreter"
	case strings.HasPrefix(name, "office-word"):
		return "office-word"
	case strings.HasPrefix(name, "office-excel"):
		return "office-excel"
	case strings.HasPrefix(name, "office-powerpoint"):
		return "office-powerpoint"
	case strings.HasPrefix(name, "pdf_"):
		return "pdf"
	case strings.HasPrefix(name, "lobe-image-designer"):
		return "image-designer"
	case strings.HasPrefix(name, "lobe-web-browsing"):
		return "web-browsing"
	case strings.HasPrefix(name, "lobe-local-system"):
		return "local-system"
	}
	return ""
}

// buildRegistry constructs only the groups referenced by the personas and
// registers them. Construction is best-effort: a group whose backend is
// unavailable (e.g. Docker for commandline) is skipped with a warning rather
// than aborting startup — personas that reference a skipped tool get a curate()
// warning so the operator notices.
func buildRegistry(ctx context.Context, personas []Persona) (*tools.ToolRegistry, error) {
	needed := map[string]bool{}
	for _, p := range personas {
		for _, t := range p.Tools {
			if g := groupForTool(t); g != "" {
				needed[g] = true
			}
		}
	}

	reg := tools.NewToolRegistry()
	for g, build := range groupBuilders {
		if !needed[g] {
			continue
		}
		ts, err := build(ctx)
		if err != nil {
			log.Printf("WARNING: tool group %q unavailable (%v); personas referencing it will lose those tools", g, err)
			continue
		}
		if err := reg.RegisterAll(ts); err != nil {
			return nil, fmt.Errorf("register group %q: %w", g, err)
		}
		log.Printf("built tool group %q (%d tools)", g, len(ts))
	}
	return reg, nil
}

// curate returns the InvokableTool slice for a persona, warning about any
// whitelisted names that are not actually registered (typos, or a tool whose
// group was not built).
func curate(reg *tools.ToolRegistry, p Persona) []tool.InvokableTool {
	curated := reg.GetByNames(p.Tools)
	if len(curated) != len(p.Tools) {
		have := map[string]bool{}
		for _, t := range curated {
			if info, err := t.Info(context.Background()); err == nil {
				have[info.Name] = true
			}
		}
		for _, want := range p.Tools {
			if !have[want] {
				log.Printf("WARNING: persona %q references unknown/unbuilt tool %q", p.ID, want)
			}
		}
	}
	return curated
}
