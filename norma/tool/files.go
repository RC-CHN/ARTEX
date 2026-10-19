package tool

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// NewRead builds the Read tool: reads a file with cat -n style line numbers and
// optional offset/limit.
func NewRead() CoreTool {
	return Build(Spec{
		Name:        "Read",
		Description: "Reads a file from the local filesystem and returns its contents with line numbers. Use offset/limit for large files. Always read a file before editing it.",
		Prompt:      "file_path may be absolute or relative to the working directory. Output lines are prefixed with their 1-based line number.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"expected_version": map[string]any{"type": "string", "description": "Version returned by Read; rejects stale file updates. Use missing to require a new file."},
				"file_path":        map[string]any{"type": "string", "description": "Path to the file to read."},
				"byte_offset":      map[string]any{"type": "integer", "description": "Continuation byte offset returned by Read; pass offset and expected_version from the same page."},
				"offset":           map[string]any{"type": "integer", "description": "1-based line to start from."},
				"limit":            map[string]any{"type": "integer", "description": "Max lines to read (default 2000)."},
			},
			"required": []any{"file_path"},
		},
		ReadOnly:    always,
		Concurrent:  always,
		Permissions: allowReadOnly,
		Run:         runRead,
	})
}

const maxLineLen = 2000

const maxReadScanBytes = 64 << 20

func runRead(ctx context.Context, input json.RawMessage, tc *ToolContext) (Result, error) {
	var in struct {
		FilePath        string `json:"file_path"`
		Offset          int    `json:"offset"`
		Limit           int    `json:"limit"`
		ByteOffset      int64  `json:"byte_offset"`
		ExpectedVersion string `json:"expected_version"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return Result{}, err
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if in.Offset < 0 || in.Limit < 0 || in.ByteOffset < 0 {
		return Fail("invalid_range", "offset, limit and byte_offset must be nonnegative", "Use the cursor returned by Read."), nil
	}
	path := resolvePath(tc, in.FilePath)
	version, info, err := currentFileVersion(path)
	if err != nil {
		return fileFailure(ctx, err)
	}
	if info == nil {
		return Fail("file_not_found", in.FilePath, "Check the path with LS or Glob."), nil
	}
	if in.ExpectedVersion != "" && in.ExpectedVersion != version {
		return fileFailure(ctx, errFileChanged)
	}
	if in.ByteOffset > 0 && in.ExpectedVersion == "" {
		return Fail("version_required", "byte_offset requires expected_version", "Pass the version and continuation cursor returned by the previous Read."), nil
	}
	f, err := os.Open(path)
	if err != nil {
		return fileFailure(ctx, err)
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return fileFailure(ctx, err)
	}
	if fileVersion(opened) != version {
		return fileFailure(ctx, errFileChanged)
	}
	sample := make([]byte, 8000)
	n, err := f.Read(sample)
	if err != nil && err != io.EOF {
		return fileFailure(ctx, err)
	}
	if isBinary(sample[:n]) {
		return Text(fmt.Sprintf("[binary file %s not shown — %d bytes; version=%s]", in.FilePath, info.Size(), version)), nil
	}
	if _, err = f.Seek(in.ByteOffset, io.SeekStart); err != nil {
		return fileFailure(ctx, err)
	}
	limit := in.Limit
	if limit == 0 {
		limit = 2000
	}
	limit = min(limit, 10000)
	start := max(in.Offset-1, 0)
	lineNo := 0
	if in.ByteOffset > 0 {
		lineNo = start
	}
	reader := bufio.NewReaderSize(f, 32<<10)
	scanned := int64(0)
	emitted := 0
	var out strings.Builder
	for emitted < limit {
		line := make([]byte, 0, maxLineLen)
		lineBytes := 0
		eof := false
		for {
			if err = ctx.Err(); err != nil {
				return Result{}, err
			}
			part, e := reader.ReadSlice('\n')
			scanned += int64(len(part))
			lineBytes += len(part)
			if scanned > maxReadScanBytes {
				return Fail("read_scan_limit", "Read reached its 64 MiB scan budget", "Use the byte_offset/version cursor from an earlier page, Grep, or a bounded streaming script for huge lines or distant offsets."), nil
			}
			if lineNo >= start && len(line) < maxLineLen {
				line = append(line, part[:min(len(part), maxLineLen-len(line))]...)
			}
			if e == bufio.ErrBufferFull {
				continue
			}
			if e != nil && !errors.Is(e, io.EOF) {
				return fileFailure(ctx, e)
			}
			eof = e == io.EOF
			break
		}
		if lineBytes == 0 && eof {
			break
		}
		if lineNo >= start {
			text := strings.TrimSuffix(string(line), "\n")
			if lineBytes > maxLineLen && !(lineBytes == maxLineLen+1 && len(line) == maxLineLen) {
				text += "… [line truncated]"
			}
			fmt.Fprintf(&out, "%6d\t%s\n", lineNo+1, text)
			emitted++
		}
		lineNo++
		if eof || out.Len() >= maxOut(tc)+readOutputBonus {
			break
		}
	}
	current, _, err := currentFileVersion(path)
	if err != nil {
		return fileFailure(ctx, err)
	}
	if current != version {
		return fileFailure(ctx, errFileChanged)
	}
	if err = ctx.Err(); err != nil {
		return Result{}, err
	}
	if out.Len() == 0 {
		if info.Size() == 0 {
			out.WriteString("(file is empty)\n")
		} else {
			out.WriteString("(no lines in the requested range)\n")
		}
	}
	body := readCapture(tc, out.String(), start)
	// If the output cap cut the last line, resume at that line, not past it.
	_, returned := headWithinBudget(out.String(), maxOut(tc)+readOutputBonus)
	if returned < emitted {
		return Text(fmt.Sprintf("[version=%s]\n%s", version, body)), nil
	}
	cursor := in.ByteOffset + scanned
	if cursor < info.Size() {
		body += fmt.Sprintf("\n[Continue reading with Read offset=%d, byte_offset=%d, expected_version=%s.]", lineNo+1, cursor, version)
	}
	return Text(fmt.Sprintf("[version=%s]\n%s", version, body)), nil
}

// readOutputBonus lifts the Read tool's output budget above the shared default
// (ToolContext.MaxOutputChars): Read gets a little more room before truncating.
const readOutputBonus = 1000

// readCapture bounds Read's line-numbered output. When it exceeds the Read budget
// (MaxToolOutputChars + readOutputBonus) it cuts at a line boundary and tells the
// model the exact offset to continue from — the source file is already on disk, so
// there is no need to spill a copy: the model just pages on with Read(offset=…).
// start is the 0-based line the output began at.
func readCapture(tc *ToolContext, s string, start int) string {
	max := maxOut(tc) + readOutputBonus
	if len(s) <= max {
		return s
	}
	head, headLines := headWithinBudget(s, max)
	nextOffset := start + headLines + 1 // 1-based file line to continue from
	return head + fmt.Sprintf(
		"\n... [Read truncated to fit the output limit. Continue reading the file with Read offset=%d (and an optional limit), or narrow with Grep/`sed -n`.] ...",
		nextOffset)
}

// headWithinBudget returns the largest whole-line prefix of s within max bytes and
// the number of lines it contains. Falls back to a hard cut if a single line
// already exceeds the budget (no newline within reach).
func headWithinBudget(s string, max int) (string, int) {
	if len(s) <= max {
		return s, strings.Count(s, "\n")
	}
	cut := strings.LastIndexByte(s[:max], '\n')
	if cut < 0 {
		return s[:max], 0
	}
	head := s[:cut+1]
	return head, strings.Count(head, "\n")
}

// isBinary reports whether data looks like a binary (non-text) file, using the
// common heuristic of a NUL byte in the first chunk.
func isBinary(data []byte) bool {
	n := len(data)
	if n > 8000 {
		n = 8000
	}
	for i := 0; i < n; i++ {
		if data[i] == 0 {
			return true
		}
	}
	return false
}

// NewWrite builds the Write tool: creates or overwrites a file.
func NewWrite() CoreTool {
	return Build(Spec{
		Name:        "Write",
		Description: "Writes a file to the local filesystem, creating parent directories and overwriting any existing file. Prefer Edit for modifying existing files.",
		Prompt:      "Provide the complete file contents — Write replaces the whole file.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"expected_version": map[string]any{"type": "string", "description": "Version returned by Read; rejects stale file updates. Use missing to require a new file."},
				"file_path":        map[string]any{"type": "string", "description": "Path of the file to write."},
				"content":          map[string]any{"type": "string", "description": "Full contents to write."},
			},
			"required": []any{"file_path", "content"},
		},
		Run: runWrite,
	})
}

func runWrite(ctx context.Context, input json.RawMessage, tc *ToolContext) (Result, error) {
	var in struct {
		FilePath        string `json:"file_path"`
		Content         string `json:"content"`
		ExpectedVersion string `json:"expected_version"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return Result{}, err
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	path := resolvePath(tc, in.FilePath)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fileFailure(ctx, err)
	}
	path, unlock, err := lockFile(ctx, path)
	if err != nil {
		return fileFailure(ctx, err)
	}
	defer unlock()
	version, info, err := currentFileVersion(path)
	if err != nil {
		return fileFailure(ctx, err)
	}
	if in.ExpectedVersion != "" && in.ExpectedVersion != version {
		return fileFailure(ctx, errFileChanged)
	}
	mode := os.FileMode(0644)
	verb := "Created"
	if info != nil {
		mode = info.Mode()
		verb = "Overwrote"
	}
	if err = commitFile(ctx, path, version, in.Content, mode); err != nil {
		return fileFailure(ctx, err)
	}
	return Text(fmt.Sprintf("%s %s (%d bytes)", verb, in.FilePath, len(in.Content))), nil
}

// NewEdit builds the Edit tool: exact string replacement with uniqueness check.
func NewEdit() CoreTool {
	return Build(Spec{
		Name:        "Edit",
		Description: "Performs an exact string replacement in a file. old_string must match exactly once unless replace_all is set. Read the file first so old_string matches precisely.",
		Prompt:      "old_string must match the file exactly (whitespace included) and be unique unless replace_all is true. new_string must differ from old_string.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"expected_version": map[string]any{"type": "string", "description": "Version returned by Read; rejects stale file updates. Use missing to require a new file."},
				"file_path":        map[string]any{"type": "string", "description": "Path of the file to edit."},
				"old_string":       map[string]any{"type": "string", "description": "Exact text to replace."},
				"new_string":       map[string]any{"type": "string", "description": "Replacement text."},
				"replace_all":      map[string]any{"type": "boolean", "description": "Replace all occurrences."},
			},
			"required": []any{"file_path", "old_string", "new_string"},
		},
		Run: runEdit,
	})
}

func runEdit(ctx context.Context, input json.RawMessage, tc *ToolContext) (Result, error) {
	var in struct {
		FilePath        string `json:"file_path"`
		OldString       string `json:"old_string"`
		NewString       string `json:"new_string"`
		ReplaceAll      bool   `json:"replace_all"`
		ExpectedVersion string `json:"expected_version"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return Result{}, err
	}
	if in.OldString == in.NewString {
		return Errorf("Error: old_string and new_string are identical"), nil
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if in.OldString == "" {
		return Fail("empty_match", "old_string must not be empty", "Read the file and provide a nonempty exact match."), nil
	}
	path, unlock, err := lockFile(ctx, resolvePath(tc, in.FilePath))
	if err != nil {
		return fileFailure(ctx, err)
	}
	defer unlock()
	version, info, err := currentFileVersion(path)
	if err != nil {
		return fileFailure(ctx, err)
	}
	if in.ExpectedVersion != "" && in.ExpectedVersion != version {
		return fileFailure(ctx, errFileChanged)
	}
	data, err := readEditFile(ctx, path, info)
	if err != nil {
		return Errorf(fmt.Sprintf("Error reading %s: %v", in.FilePath, err)), nil
	}
	content := string(data)
	count := strings.Count(content, in.OldString)
	if count == 0 {
		return Fail("match_not_found", fmt.Sprintf("old_string not found in %s", in.FilePath), "Read the current file; copy exact text including whitespace and rebuild the edit."), nil
	}
	if count > 1 && !in.ReplaceAll {
		return Fail("ambiguous_match", fmt.Sprintf("old_string is not unique in %s (%d matches)", in.FilePath, count), "Add surrounding lines to old_string; use replace_all only if every occurrence should change."), nil
	}
	var updated string
	if in.ReplaceAll {
		updated = strings.ReplaceAll(content, in.OldString, in.NewString)
	} else {
		updated = strings.Replace(content, in.OldString, in.NewString, 1)
	}
	if err := commitFile(ctx, path, version, updated, info.Mode()); err != nil {
		return fileFailure(ctx, err)
	}
	return Text(fmt.Sprintf("Edited %s (%d replacement(s))", in.FilePath, count)), nil
}
