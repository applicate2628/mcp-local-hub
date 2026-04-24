package perftools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Keep output below the daemon stdio scanner's 1 MiB token limit.
// Reserve headroom for JSON-RPC envelope + JSON escaping overhead.
const llvmObjdumpMaxStdoutBytes = 512 * 1024

// llvmObjdumpTool disassembles a binary using llvm-objdump. Unlike
// godbolt's sandbox compile, this operates on the USER'S ACTUAL
// build output — post-LTO, post-PGO, post-linker-inlining — so
// it's the authoritative answer to "what does the binary really do?".
func (tb *PerfToolbox) llvmObjdumpTool(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if !tb.tools.LLVMObjdump.Installed {
		return errResult("llvm-objdump not installed: " + tb.tools.LLVMObjdump.Error), nil
	}

	var args struct {
		Binary     string   `json:"binary"`
		Function   string   `json:"function"`
		Section    string   `json:"section"`
		WithSource bool     `json:"with_source"`
		Intel      bool     `json:"intel"`
		ExtraArgs  []string `json:"extra_args"`
	}
	if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
		return errResult(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	if args.Binary == "" {
		return errResult("missing required parameter: binary (path to a built .exe / .o / .so / .a)"), nil
	}

	var cmdArgs []string
	if args.Function != "" {
		// --disassemble-symbols limits output to the named symbol; in that
		// mode we DON'T pass a bare --disassemble (that would expand to the
		// whole .text section and undo the filter).
		cmdArgs = append(cmdArgs, "--disassemble-symbols="+args.Function)
	} else {
		cmdArgs = append(cmdArgs, "--disassemble")
	}
	cmdArgs = append(cmdArgs, "--demangle", "--print-imm-hex")
	if args.Section != "" {
		cmdArgs = append(cmdArgs, "--section="+args.Section)
	}
	if args.WithSource {
		cmdArgs = append(cmdArgs, "--source")
	}
	if args.Intel {
		cmdArgs = append(cmdArgs, "--x86-asm-syntax=intel")
	}
	cmdArgs = append(cmdArgs, args.ExtraArgs...)
	cmdArgs = append(cmdArgs, args.Binary)

	cap, err := runCaptureLimited(ctx, tb.tools.LLVMObjdump.Path, "", cmdArgs, llvmObjdumpMaxStdoutBytes, 512*1024)
	if err != nil {
		if errors.Is(err, errOutputLimitExceeded) {
			return errResult(fmt.Sprintf("llvm-objdump output exceeded %d bytes; narrow the request with function/section filters", llvmObjdumpMaxStdoutBytes)), nil
		}
		return errResult(fmt.Sprintf("llvm-objdump failed: %v", err)), nil
	}
	if cap.ExitCode != 0 {
		return errResult(fmt.Sprintf("llvm-objdump exited %d\nstderr:\n%s", cap.ExitCode, string(cap.Stderr))), nil
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(cap.Stdout)}},
	}, nil
}
