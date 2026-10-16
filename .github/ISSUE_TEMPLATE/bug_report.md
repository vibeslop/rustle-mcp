---
name: Bug report
about: A tool returned an error or a wrong result
---

**Tool and arguments**
The tool you called and its arguments, e.g. `watch_video` with `{"source": "...", "frames": 12}`.

**Source**
A link, or how to make the file (an ffmpeg command, a public video). Please don't attach other people's videos.

**What you expected, and what you got**
The result's first line (its metadata) and any error code.

**Environment**
`rustle-mcp --version`, `ffmpeg -version | head -1`, your OS, and the client (Claude Code, Codex, Cursor...).

A log with `RUSTLE_TRACE=1` set in the server's environment helps: it lists every ffmpeg run and request.
