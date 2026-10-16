# AGENTS.md

You are changing Rustle, a stdio MCP server that lets agents read videos: transcripts, frames and sound from YouTube, web pages, media URLs and local files. The connected model does the reading, looking and listening; Rustle fetches, decodes and sizes what the model is handed. It is written in Go, ships as one static binary per platform with no dependencies beyond Go's standard library, and runs on the user's machine next to their agent. Hosting it for others is a non-goal: YouTube treats requests from data centers as bots.

The README is for people deciding to use Rustle. This file is for changing it.

## What we never compromise on

1. **Tokens.** Every result is sized for what the model is billed. Images are planned in Claude's 28 px patches (one token each, `ImageTokens` in `internal/rustle/client.go`), transcripts default to a digest, and metadata is one line of compact JSON. Tool descriptions and schemas are sent with every request, so `internal/tools/server_test.go` caps their size; a new argument has to earn its characters.
2. **What Rustle draws is what the model sees.** Harnesses resize and re-encode images past their limits (Claude Code: over 2000 px or 3.75 MB; Codex: shrinks to a 768 px short side unless asked for the original). Keep images inside them, or the model gets a worse picture than the one you tested.
3. **Speed.** The server answers its client in a few milliseconds, and most calls take tens to hundreds of milliseconds. The costs are starting ffmpeg, decoding, and network round trips; measure before and after a change that touches them (`RUSTLE_DEBUG=1` puts each call's milliseconds in its metadata, `RUSTLE_TRACE=1` logs every ffmpeg run and request).
4. **Small.** Standard library only, one binary of about 7 MB. Prefer a few lines of code to a package.
5. **Private.** No telemetry, no keys, no inference API. Rustle contacts only the sources an agent asks for, plus YouTube's visitor id at startup.

Prefer simple systems and software that feels obvious. Find the real constraint, then make the smallest change that makes the behaviour unsurprising. Don't keep complexity because it exists, and don't add machinery because it looks thorough. These are defaults, not rules: the developer you are working with can override any of them.

## Glossary

- **Source**: what a tool's `source` argument names: a YouTube link or id, a web page, a media or HLS URL, or a local path. Resolved once into a **record** (`rustle.Record`) and cached.
- **Sheet** and **cell**: a contact-sheet image from `watch_video`, and one frame in it, labelled `#n m:ss.ss`. Consecutive near-identical frames share one cell (`#3-5 ...`).
- **Crop**: `[x, y, w, h]` as fractions of the frame from the top left. A crop gets the whole frame's pixel budget and is enlarged up to 4x when small.
- **Digest**: the default transcript, the key sentences verbatim, about a third of the words.
- **Patch**: Claude's 28 x 28 px image unit, one token.
- **Client** or **harness**: the app running the agent: Claude Code, Codex, Cursor. Its name picks a profile in `internal/rustle/client.go`.
- **Window**: a few seconds of a YouTube stream fetched by byte range, cached, and handed to ffmpeg.
- **Storyboard**: YouTube's grid of small thumbnails, used for cheap overviews.
- **Warm process**: an ffmpeg started ahead of a call and waiting for its options (`internal/proc/warm.go`).

## Where things are

- `main.go`: startup and the CLI (`--help`, `--version`); `setup.go`: `rustle-mcp setup`, which registers the binary with a client.
- `internal/mcp`: the stdio JSON-RPC transport. `internal/tools`: tool schemas, argument parsing, how results are rendered, and one file per tool.
- `internal/source`: turning a source into a record (`Identify`, `Resolve`, `Probed`, `MediaInput`), web pages and local folders.
- `internal/youtube`: player requests, captions, DASH windows, storyboards, search.
- `internal/media`: ffmpeg commands, frame sampling, sheet layout, the MP4 index reader. `internal/imaging`: the JPEG and PNG encoders and the bitmap font. `internal/audio`: analysis, FFT, spectrogram.
- `internal/transcript`: caption parsing, paragraphs, search and the digest.
- `internal/rustle`: what every package shares: settings, errors, records, the cache, ordered metadata (`Obj`, `Compact`). `internal/proc`: running processes; `internal/fetch`: HTTP; `internal/jsonv`: order-preserving JSON for YouTube's responses.
- Tests sit next to the code (`*_test.go`), offline; fixtures are generated where possible (`internal/testmedia`). `scripts/smoke`: every tool against live YouTube.

## Commands

```sh
go build -o rustle-mcp .    # the binary .mcp.json points at
go vet ./...
go test ./...               # offline, a few seconds; frame tests skip without ffmpeg
go run ./scripts/smoke      # builds, then calls every tool on live YouTube; needs the network
```

To release, tag the commit and push the tag: `git tag v1.2.3 && git push origin v1.2.3`. The tag starts `.github/workflows/release.yml`, which runs CI, cross-compiles a binary per platform and publishes them, with checksums, as a GitHub release. `install.sh` installs the latest one.

## Ways to hurt yourself

- **Fixing one source path.** The most common defect here is a change that works on the file you tested and is missing everywhere else. Sources take different paths: local MP4 and MOV (keyframes read from the file's own index), other local files (ffmpeg seeks), MPEG-TS (no index; seeks overshoot), YouTube (byte-range windows and storyboards), direct URLs and HLS, and audio-only files. Before calling a media change done, walk the list. `internal/tools/watch_test.go`, `frame_test.go` and `internal/youtube/media_test.go` cover most of it.
- **Fused multiply-adds.** On arm64 Go may compute `a*b + c` in one instruction that skips the product's rounding, so a time or size can land a millisecond or a pixel off. Where a product feeds a sum and the result is shown or compared, round the product first with an explicit conversion: `float64(a*b) + c`. `rustle.Round` does this for its argument.
- **Changing a published record.** Records are shared by concurrent calls; code that learns more about a source stores a changed copy (`record.Copy()`, then `rustle.SaveRecord`), never edits the one it was handed.
- **Testing an old build.** Clients run the binary, not the source. Rebuild and reconnect the server before judging a change through a client. Don't kill MCP server processes you didn't start: you may be running inside the client that owns them.
- **Letting the tool surface drift.** Every character in a description or schema is paid on every request. Put guidance in a result's `hint` or notes, where it costs only when it applies.
- **Changing an image size casually.** It changes the token cost and can cross a harness limit. Check `ImageTokens` and the harness limits above, and keep sheets in whole patches.
- **Network in tests.** Tests must pass offline; stub requests with `fetch.SetTransport` as the existing tests do. Live checks belong in `scripts/smoke`.
- **Third-party content in the repo.** Don't commit real transcripts, lyrics or other people's video files as fixtures. Generate them.

## Conventions

- Comments say why the code is this way, not what it does. Keep them true when the code changes.
- Errors an agent sees are `*rustle.Error`s with a `Code`, `Retryable`, and a `Recovery` sentence that names the fix the agent should make.
- Arguments are forgiving: clamp or ignore with a note in the result rather than fail.
- Calls run concurrently: package-level state needs a lock, and work several callers share runs detached from any one caller's cancellation (`rustle.Flight`, `rustle.Detach`).
- The README describes behaviour for users; internals go in code comments or here.
- A pull request title says what changed and why it matters, for example "watch_video: follow one region with crop". Run `gofmt`, `go vet ./...` and `go test ./...`, and `go run ./scripts/smoke` when YouTube code changed.
