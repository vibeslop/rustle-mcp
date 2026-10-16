# Contributing

Thanks for helping. Rustle is small on purpose, so the best changes are focused ones: a bug fixed where it starts, or a feature that earns the tokens and the code it costs.

## Before you start

For anything beyond a small fix, open an issue first and describe what you want to change and why. It saves you writing code that takes a different direction from the project.

[AGENTS.md](AGENTS.md) explains how the code is laid out, what the project never compromises on (tokens, image fidelity, speed, size, privacy) and the mistakes that are easy to make. Read it before changing media or YouTube code.

## Making a change

You need Go 1.24 or newer, and ffmpeg 7 or newer for the frame and sound tests.

```sh
go build -o rustle-mcp .
go vet ./...
go test ./...
go run ./scripts/smoke    # when YouTube code changed; needs the network
```

- Keep tests offline: stub HTTP with `fetch.SetTransport`, and generate media with ffmpeg rather than committing recordings.
- Measure before and after any change to decoding, sampling or requests (`RUSTLE_DEBUG=1` reports each call's time).
- Run `gofmt`; CI checks it.

## Pull requests

One change per pull request. Its title says what changed and why it matters, for example "watch_video: follow one region with crop". Describe how you checked it, and say which source types you tried: local MP4, other local files, MPEG-TS, YouTube, URLs, audio-only.

## Reporting bugs

Open an issue with the tool, its arguments, the source (or a way to make one), what you expected and what you got. Set `RUSTLE_TRACE=1` in the server's environment to log every ffmpeg run and request to stderr.

Security issues go through GitHub's private reporting instead; see [SECURITY.md](SECURITY.md).
