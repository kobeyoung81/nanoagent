package tool

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// lineWindow is one page of a file: the lines collected, where they sit, and the
// flags that explain why the page is not the whole file.
type lineWindow struct {
	Body     string
	Offset   int // first line requested (1-based)
	Start    int // first line actually returned (0 = nothing returned)
	End      int // last line actually returned
	Total    int // total lines (meaningful only when Counted)
	Counted  bool
	BodyFull bool // the byte cap stopped the body before the line limit
	LongLine bool // at least one line was truncated at maxReadLineBytes
	HasNul   bool // the returned window holds NUL bytes
	note     []string
}

// intParam reads an integer parameter that may arrive as a JSON number, a Go int
// or a string (providers differ).
func intParam(params map[string]any, name string) int {
	switch v := params[name].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	return 0
}

// boolParam reads a boolean parameter that may arrive as a JSON bool or a
// string (providers differ in how they render true/false).
func boolParam(params map[string]any, name string) bool {
	switch v := params[name].(type) {
	case bool:
		return v
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "true", "1", "yes":
			return true
		}
	}
	return false
}

// sniffHead classifies the head of an open file. It consumes at most
// sniffSampleBytes of the file's current offset, so callers must seek back to
// the start before reading the body.
func sniffHead(f *os.File, size int64) contentKind {
	n := int64(sniffSampleBytes)
	if size < n {
		n = size
	}
	if n <= 0 {
		return contentKind{Sniffed: "text/plain; charset=utf-8"}
	}
	sample := make([]byte, n)
	read, _ := io.ReadFull(f, sample)
	return sniffContent(sample[:read])
}

// binaryReport is the structured answer to a binary read: what the file is, that
// it was not returned, and what to do instead.
func binaryReport(path string, size int64, kind contentKind) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: not read — %s (%s, %d bytes).", path, kind.Reason, kind.Sniffed, size)
	if note := mismatchNote(path, kind); note != "" {
		fmt.Fprintf(&b, " Note: %s.", note)
	}
	b.WriteString(" This tool returns text only: for an image or a document, attach it instead; " +
		"to inspect the bytes use `file`, `ls -l` or `head -c 64` (the shell tools can both show it).")
	return b.String()
}

// readEncodedFile serves a window of a UTF-16/32 document. The decode needs the
// whole file, so it is bounded and says so when the file is too large. safe is
// the resolved path to open; display is the path the caller asked for.
func readEncodedFile(safe, display string, size int64, kind contentKind, offset, limit, capBytes int) (string, error) {
	if size > encodedReadMaxSize {
		return fmt.Sprintf("%s: %s text, %d bytes — too large to convert in one call; convert it first "+
			"(e.g. `iconv -f utf-16 -t utf-8`) and read the result.", display, kind.Encoding, size), nil
	}
	data, err := os.ReadFile(safe)
	if err != nil {
		return "", err
	}
	text := decodeEncoded(data, kind.Encoding)
	window, err := readLinesWindow(strings.NewReader(text), true, offset, limit, capBytes)
	if err != nil {
		return "", err
	}
	window.note = append(window.note, "decoded from "+kind.Encoding)
	return windowResult(display, size, kind, window), nil
}

// readLinesWindow collects at most limit lines starting at offset (1-based),
// stopping the body once capBytes would be exceeded. countLines streams the
// reader to the end so the footer can name the total.
func readLinesWindow(r io.Reader, countLines bool, offset, limit, capBytes int) (lineWindow, error) {
	w := lineWindow{Offset: offset, Counted: countLines}
	br := bufio.NewReaderSize(r, 64*1024)
	var body strings.Builder
	lineNo := 0
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			lineNo++
			if countLines {
				w.Total = lineNo
			}
			if lineNo >= offset && !w.BodyFull && w.End < offset+limit-1 {
				hasNewline := strings.HasSuffix(line, "\n")
				text := strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
				if lineNo == offset {
					text = strings.TrimPrefix(text, "\ufeff")
				}
				if len(text) > maxReadLineBytes {
					text = text[:maxReadLineBytes]
					w.LongLine = true
				}
				if strings.IndexByte(text, 0) >= 0 {
					w.HasNul = true
				}
				if body.Len() > 0 && body.Len()+len(text)+1 > capBytes {
					w.BodyFull = true
				} else {
					if w.Start == 0 {
						w.Start = lineNo
					}
					body.WriteString(text)
					// Mirror the source: a final line without a newline stays
					// without one, so a small file reads back byte-identical.
					if hasNewline {
						body.WriteByte('\n')
					}
					w.End = lineNo
				}
			}
		}
		if err != nil {
			if err != io.EOF {
				w.Body = body.String()
				return w, err
			}
			break
		}
		if !countLines && (w.BodyFull || w.End >= offset+limit-1) {
			break
		}
	}
	w.Body = body.String()
	return w, nil
}

// windowResult renders a page plus the footer that makes it resumable. A small
// file that fits in one page comes back exactly as before (no footer), except
// when something needs saying — a truncated long line, or an extension that
// disagrees with the content.
func windowResult(path string, size int64, kind contentKind, w lineWindow) string {
	if w.Counted && w.Total == 0 {
		return w.Body
	}
	atEnd := w.Counted && w.End >= w.Total
	wholeFile := w.Start == 1 && !w.BodyFull && !w.LongLine && !w.HasNul && atEnd
	notes := append([]string(nil), w.note...)
	if note := mismatchNote(path, kind); note != "" {
		notes = append(notes, note)
	}
	if wholeFile && len(notes) == 0 {
		return w.Body
	}

	var b strings.Builder
	b.WriteString(w.Body)
	if w.Body != "" && !strings.HasSuffix(w.Body, "\n") {
		b.WriteString("\n")
	}
	total := "?"
	if w.Counted {
		total = strconv.Itoa(w.Total)
	}
	switch {
	case w.Start == 0:
		fmt.Fprintf(&b, "[no lines returned: the file has %s lines, so offset %d is past the end; file %d bytes]\n",
			total, w.Offset, size)
	case atEnd:
		fmt.Fprintf(&b, "[end of file: lines %d-%d of %s; file %d bytes]\n", w.Start, w.End, total, size)
	default:
		fmt.Fprintf(&b, "[window: lines %d-%d of %s; continue with offset=%d; file %d bytes]\n",
			w.Start, w.End, total, w.End+1, size)
	}
	if w.LongLine {
		fmt.Fprintf(&b, "[a line was truncated at %d bytes]\n", maxReadLineBytes)
	}
	if w.HasNul {
		b.WriteString("[the window contains NUL bytes — the file may be binary beyond the sampled head]\n")
	}
	if w.BodyFull {
		fmt.Fprintf(&b, "[byte cap reached: continue with offset=%d]\n", w.End+1)
	}
	for _, note := range notes {
		fmt.Fprintf(&b, "[%s]\n", note)
	}
	return b.String()
}
