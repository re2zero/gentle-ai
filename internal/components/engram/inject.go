package engram

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/gentleman-programming/gentle-ai/internal/agents"
	"github.com/gentleman-programming/gentle-ai/internal/assets"
	"github.com/gentleman-programming/gentle-ai/internal/components/filemerge"
	"github.com/gentleman-programming/gentle-ai/internal/model"
)

type InjectionResult struct {
	Changed bool
	Files   []string
}

// defaultEngramServerJSON is the MCP server config for separate-file strategy (Claude Code).
// Uses --tools=agent per engram contract.
var defaultEngramServerJSON = []byte("{\n  \"command\": \"engram\",\n  \"args\": [\"mcp\", \"--tools=agent\"]\n}\n")

// defaultEngramOverlayJSON is the settings.json overlay for merge strategy (Gemini, etc.).
// Uses --tools=agent per engram contract.
var defaultEngramOverlayJSON = []byte("{\n  \"mcpServers\": {\n    \"engram\": {\n      \"command\": \"engram\",\n      \"args\": [\"mcp\", \"--tools=agent\"]\n    }\n  }\n}\n")

// openCodeEngramOverlayJSON is the opencode.json overlay using the new MCP format.
// Uses --tools=agent in the command array per engram contract.
var openCodeEngramOverlayJSON = []byte("{\n  \"mcp\": {\n    \"engram\": {\n      \"command\": [\"engram\", \"mcp\", \"--tools=agent\"],\n      \"enabled\": true,\n      \"type\": \"local\"\n    }\n  }\n}\n")

// vsCodeEngramOverlayJSON is the VS Code mcp.json overlay using the "servers" key.
// Uses --tools=agent per engram contract.
var vsCodeEngramOverlayJSON = []byte("{\n  \"servers\": {\n    \"engram\": {\n      \"command\": \"engram\",\n      \"args\": [\"mcp\", \"--tools=agent\"]\n    }\n  }\n}\n")

func Inject(homeDir string, adapter agents.Adapter) (InjectionResult, error) {
	if !adapter.SupportsMCP() {
		return InjectionResult{}, nil
	}

	files := make([]string, 0, 2)
	changed := false

	// 1. Write MCP server config using the adapter's strategy.
	switch adapter.MCPStrategy() {
	case model.StrategySeparateMCPFiles:
		// Engram v1.10.3+ writes an absolute path for the command field when
		// `engram setup <agent>` is invoked. gentle-ai's Inject() runs after
		// engram setup, so we must preserve any absolute command path already
		// present instead of silently overwriting it with the relative "engram".
		// See: https://github.com/Gentleman-Programming/gentle-ai/issues (engram absolute path regression)
		mcpPath := adapter.MCPConfigPath(homeDir, "engram")
		content := buildSeparateMCPContent(mcpPath, defaultEngramServerJSON)
		mcpWrite, err := filemerge.WriteFileAtomic(mcpPath, content, 0o644)
		if err != nil {
			return InjectionResult{}, err
		}
		changed = changed || mcpWrite.Changed
		files = append(files, mcpPath)

	case model.StrategyMergeIntoSettings:
		settingsPath := adapter.SettingsPath(homeDir)
		if settingsPath == "" {
			break
		}
		overlay := defaultEngramOverlayJSON
		if adapter.Agent() == model.AgentOpenCode {
			overlay = openCodeEngramOverlayJSON
		}
		settingsWrite, err := mergeJSONFile(settingsPath, overlay)
		if err != nil {
			return InjectionResult{}, err
		}
		changed = changed || settingsWrite.Changed
		files = append(files, settingsPath)

	case model.StrategyMCPConfigFile:
		mcpPath := adapter.MCPConfigPath(homeDir, "engram")
		if mcpPath == "" {
			break
		}
		overlay := defaultEngramOverlayJSON
		if adapter.Agent() == model.AgentVSCodeCopilot {
			overlay = vsCodeEngramOverlayJSON
		}

		mcpWrite, err := mergeJSONFile(mcpPath, overlay)
		if err != nil {
			return InjectionResult{}, err
		}
		changed = changed || mcpWrite.Changed
		files = append(files, mcpPath)

	case model.StrategyTOMLFile:
		// Codex: upsert [mcp_servers.engram] block and instruction-file keys
		// in ~/.codex/config.toml, then write instruction files.
		// All TOML mutations are composed in a single pass before writing to
		// ensure idempotency (no intermediate states that differ on re-run).
		configPath := adapter.MCPConfigPath(homeDir, "engram")
		if configPath == "" {
			break
		}

		// Determine instruction file paths before mutating the config.
		instructionsPath, compactPath, instrErr := writeCodexInstructionFiles(homeDir)
		if instrErr != nil {
			return InjectionResult{}, instrErr
		}

		// Read existing config and apply all mutations in one pass.
		existing, err := readFileOrEmpty(configPath)
		if err != nil {
			return InjectionResult{}, err
		}
		withMCP := filemerge.UpsertCodexEngramBlock(existing)
		withInstr := filemerge.UpsertTopLevelTOMLString(withMCP, "model_instructions_file", instructionsPath)
		withCompact := filemerge.UpsertTopLevelTOMLString(withInstr, "experimental_compact_prompt_file", compactPath)

		tomlWrite, err := filemerge.WriteFileAtomic(configPath, []byte(withCompact), 0o644)
		if err != nil {
			return InjectionResult{}, err
		}
		changed = changed || tomlWrite.Changed
		files = append(files, configPath)
	}

	// 2. Inject Engram memory protocol into system prompt (if supported).
	if adapter.SupportsSystemPrompt() {
		switch adapter.SystemPromptStrategy() {
		case model.StrategyMarkdownSections:
			promptPath := adapter.SystemPromptFile(homeDir)
			protocolContent := assets.MustRead("claude/engram-protocol.md")

			existing, err := readFileOrEmpty(promptPath)
			if err != nil {
				return InjectionResult{}, err
			}

			updated := filemerge.InjectMarkdownSection(existing, "engram-protocol", protocolContent)

			mdWrite, err := filemerge.WriteFileAtomic(promptPath, []byte(updated), 0o644)
			if err != nil {
				return InjectionResult{}, err
			}
			changed = changed || mdWrite.Changed
			files = append(files, promptPath)

		default:
			promptPath := adapter.SystemPromptFile(homeDir)
			protocolContent := assets.MustRead("claude/engram-protocol.md")

			existing, err := readFileOrEmpty(promptPath)
			if err != nil {
				return InjectionResult{}, err
			}

			updated := filemerge.InjectMarkdownSection(existing, "engram-protocol", protocolContent)

			mdWrite, err := filemerge.WriteFileAtomic(promptPath, []byte(updated), 0o644)
			if err != nil {
				return InjectionResult{}, err
			}
			changed = changed || mdWrite.Changed
			files = append(files, promptPath)
		}
	}

	return InjectionResult{Changed: changed, Files: files}, nil
}

// writeCodexInstructionFiles writes the Engram memory protocol and compact prompt
// files to ~/.codex/ and returns their paths.
func writeCodexInstructionFiles(homeDir string) (instructionsPath, compactPath string, err error) {
	codexDir := homeDir + "/.codex"
	instructionsPath = codexDir + "/engram-instructions.md"
	compactPath = codexDir + "/engram-compact-prompt.md"

	instrContent := assets.MustRead("codex/engram-instructions.md")
	instrWrite, err := filemerge.WriteFileAtomic(instructionsPath, []byte(instrContent), 0o644)
	if err != nil {
		return "", "", fmt.Errorf("write codex engram-instructions.md: %w", err)
	}
	_ = instrWrite

	compactContent := assets.MustRead("codex/engram-compact-prompt.md")
	compactWrite, err := filemerge.WriteFileAtomic(compactPath, []byte(compactContent), 0o644)
	if err != nil {
		return "", "", fmt.Errorf("write codex engram-compact-prompt.md: %w", err)
	}
	_ = compactWrite

	return instructionsPath, compactPath, nil
}

func mergeJSONFile(path string, overlay []byte) (filemerge.WriteResult, error) {
	baseJSON, err := osReadFile(path)
	if err != nil {
		return filemerge.WriteResult{}, err
	}

	merged, err := filemerge.MergeJSONObjects(baseJSON, overlay)
	if err != nil {
		return filemerge.WriteResult{}, err
	}

	return filemerge.WriteFileAtomic(path, merged, 0o644)
}

var osReadFile = func(path string) ([]byte, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read json file %q: %w", path, err)
	}

	return content, nil
}

func readFileOrEmpty(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("read file %q: %w", path, err)
	}
	return string(data), nil
}

// buildSeparateMCPContent returns the content to write to the MCP server JSON
// file for agents that use the StrategySeparateMCPFiles strategy (e.g. Claude
// Code).
//
// Engram v1.10.3+ writes an absolute command path when `engram setup` is run.
// gentle-ai runs Inject() after setup, so we must not overwrite that absolute
// path with the relative "engram" string from defaultEngramServerJSON.
//
// Logic:
//   - If the file does not exist yet, return defaultContent unchanged.
//   - If the file exists but cannot be parsed as JSON, return defaultContent.
//   - If the parsed JSON has a "command" value that is an absolute path to the
//     engram binary, rebuild the config using that command and the canonical
//     args (["mcp", "--tools=agent"]) so that the absolute path is preserved
//     and the correct flags are always present.
//   - Otherwise (relative command or other value), return defaultContent.
func buildSeparateMCPContent(mcpPath string, defaultContent []byte) []byte {
	raw, err := os.ReadFile(mcpPath)
	if err != nil {
		// File does not exist or is not readable — use the default.
		return defaultContent
	}

	var existing map[string]any
	if err := json.Unmarshal(raw, &existing); err != nil {
		// Malformed JSON — use the default.
		return defaultContent
	}

	cmd, ok := existing["command"].(string)
	if !ok || !isAbsoluteEngramPath(cmd) {
		// No command, or not an absolute path — use the default.
		return defaultContent
	}

	// Rebuild with the preserved absolute command and the canonical args.
	rebuilt := map[string]any{
		"command": cmd,
		"args":    []string{"mcp", "--tools=agent"},
	}
	encoded, err := json.MarshalIndent(rebuilt, "", "  ")
	if err != nil {
		// Should be impossible with a plain map — use the default as fallback.
		return defaultContent
	}
	return append(encoded, '\n')
}

// isAbsoluteEngramPath reports whether path is an absolute filesystem path
// that points to an engram binary.
//
// Engram setup writes the full resolved path of the binary it was invoked
// from, so any absolute path ending in "engram" (Unix) or "engram.exe"
// (Windows) is considered valid.
func isAbsoluteEngramPath(path string) bool {
	if !filepath.IsAbs(path) {
		return false
	}
	base := filepath.Base(path)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(base, "engram.exe") || strings.EqualFold(base, "engram")
	}
	return base == "engram"
}
