# Rustle

[![CI](https://github.com/vibeslop/rustle-mcp/actions/workflows/ci.yml/badge.svg)](https://github.com/vibeslop/rustle-mcp/actions/workflows/ci.yml)

Rustle is an MCP server for AI agents to watch videos. Instead of letting the agent look at frames or read a transcript, it offers both at the same time with `watch_video`.

You can send the agent YouTube and website links, as well as video files. Here is what you can ask it:

1. Find me a guide for my situation based on your memories.
2. I want to buy a new laptop. Watch several reviews and do online research. Ignore what you think is paid or provides low value. Then suggest which laptop to buy regardless of the budget.

## Install

Install it locally, or [host it on your own server](docs/hosting.md).

Rustle requires no API key and has no telemetry. The videos are being analyzed by AI agents without requiring separate services.

On macOS and Linux:

```sh
curl -fsSL https://raw.githubusercontent.com/vibeslop/rustle-mcp/main/install.sh | sh
```

On Windows, in PowerShell:

```powershell
irm https://raw.githubusercontent.com/vibeslop/rustle-mcp/main/install.ps1 | iex
```

Anywhere else, with Go installed:

```sh
go run github.com/vibeslop/rustle-mcp@latest setup
```

## Documentation

- [Hosting](docs/hosting.md)
- [Configuration](docs/configuration.md)

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

[MIT](LICENSE)
