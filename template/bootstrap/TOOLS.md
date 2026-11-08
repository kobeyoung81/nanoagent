# Tool Notes

- Workspace skills live under `/workspace/skills/`.
- `md2pdf` is available for Markdown-to-PDF conversion.
  - Syntax-highlighting definitions are bundled at `/usr/share/md2pdf/syntax_files`.
  - A Chinese (GB) font is bundled at `/usr/share/fonts/md2pdf/ArphicGB.ttf` and auto-discovered — no `--font-file` needed for CJK text.
- `officecli` is available for `.docx`, `.xlsx`, and `.pptx` inspection and editing.
- When a skill folder includes extra markdown files beside `SKILL.md`, read those files directly when you need the detailed workflow.

## Calling tools: one at a time, or several at once

- Tool calls written in **one assistant message run one after another**, in
  order. That is the default and it is right for dependent steps.
- Independent calls can run **at the same time** when they are wrapped in one
  `parallel_execution` call: `calls: [{ tool, arguments }, ...]`, up to 8.
  - `arguments` is that tool's own arguments as a **JSON object** — pass the
    object itself, never a JSON string of it. A newline inside a command must
    be written as `\n`, and a quote or a backslash inside one is escaped
    (`\"`, `\\`).
  - Results come back in request order, one
    `<call index=… tool=… state=… elapsed_ms=…>` block each, and one failing
    call never stops the others.
  - This is the tool for several reads or greps, and for several `task`
    subagents: a single subagent can take minutes, so N subagents written as N
    messages means N × minutes of waiting.
  - A few calls cannot be batched and are answered inside their own block:
    `question`, `todowrite`, `plan_write`, `plan_execute`, `cron`, the media
    tools, and any call the mode would ask you to approve (call those on their
    own so the approval card can be shown).
  - A batch is for **independent** calls. If two entries write the same file
    (`write`, `edit`, `append_note`), the second one is refused: those calls
    depend on each other, so run them one after another in separate messages.

## Reading files

- `read` returns one window (default 2000 lines, and a byte budget) followed by
  a footer naming the line range and the offset that continues it: page through
  a large file with `offset` instead of asking for it from the top again.
- Binary content (images, archives, databases) is refused with a report instead
  of being returned; a UTF-16/32 document with a byte-order mark is decoded.
- To find something in a large file, locate it first (`grep -n`) and read around
  the hit. Prefer `grep`/`sed -n 'A,Bp'`/`head`/`tail` over reading everything.
- Shell output is bounded (head + tail, with an omission marker), so piping a
  huge file through a command loses the middle — page with `read` instead.

## Delegating to subagents

- `task` starts one subagent and waits for it; `readonly_task` does the same with
  the read-only subagent types only.
- A subagent starts with no conversation history and **cannot start another
  subagent**: give it a complete, self-contained prompt.
- Tell the subagent whether you expect changes or research, and how to verify.

## Paths and the shell

- Your **workspace is your starting point**: both the interactive terminal and the
  `bash` tool begin in your own working directory (`<user home>/agents/<you>`).
- In the **interactive terminal** `$HOME` is the container user's home, not your
  workspace: `cd` with no argument (and `cd ~`) lands in the shared user home, and
  `~/.ssh`, `~/.gitconfig`, `~/.npmrc` are the user's own files there.
- In the **`bash` tool** `$HOME` is your workspace instead, so tool caches and
  configuration stay per-agent. The two differ on purpose — do not try to "fix"
  it by reassigning `$HOME`.

## Python

- `pip3 install <pkg>` fails with `externally-managed-environment`. Ubuntu 24.04
  marks the system interpreter as managed (PEP 668), so pip refuses to touch it.
  Either work in a virtual environment
  (`python3 -m venv .venv && . .venv/bin/activate && pip install …`) or, for a
  throwaway tool, pass `--break-system-packages` and accept that apt may later
  overwrite what you installed.
- There is no `python` command, only `python3`.
