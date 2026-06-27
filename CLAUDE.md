# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

ElectronStudio is a cross-platform voice assistant for the [ElectronBot](https://github.com/peng-zhihui/ElectronBot) desktop robot. The backend is **pure Go** (`CGO_ENABLED=0`, cross-compiles to macOS / Windows / Raspberry Pi); the frontend is an embedded web SPA. Native capabilities that would normally need a C toolchain are isolated into either **sidecar processes** (Python: speech, gesture) or **purego runtime loading** (libusb for the real robot). Comments and docs are in Chinese.

## Commands

```bash
# Build the main binary (no C toolchain needed)
CGO_ENABLED=0 go build ./cmd/electronstudio

# Cross-compile (e.g. Raspberry Pi arm64)
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build ./cmd/electronstudio

# Run the minimal loop — Mock robot + local Echo model, no hardware/network/cgo
go run ./cmd/electronstudio -addr :8080      # connect a browser to http://localhost:8080/

# Attach a real LLM via env (still pure HTTP, no cgo)
OPENAI_BASE_URL=http://localhost:11434/v1 OPENAI_MODEL=qwen2.5 go run ./cmd/electronstudio

# Tests
go test ./...
go test ./internal/music                          # one package
go test ./internal/music -run TestSearchAndPlay    # one test
go vet ./...
```

**One-click launch** (`scripts/run.sh` on Linux/macOS, `scripts/run.ps1` / `run.bat` on Windows) bootstraps a Python venv, installs sidecar deps, downloads speech models (~hundreds of MB on first run), optionally builds, then starts the speech sidecar + main program together. Flags: `--no-sidecar` (text-only, skips Python/models), `--build`, `--addr :8099`, `--mirror <url>` (China mirror for model downloads). It never touches an existing `config.json`.

## Architecture

### Assembly point: `cmd/electronstudio`
`main.go`'s `newApp` wires every module together and holds runtime state on the `app` struct. The runtime is a set of goroutines fed by channels:

- **`dispatchLoop`** consumes inbound WebSocket commands (`server.Inbound()`) → `handle()` switches on `protocol.Type`.
- **`speechLoop`** consumes sidecar speech events (wake/VAD/ASR); a final ASR result enters the same chat path as typed text.
- **`gestureLoop`** maps gesture events to robot behaviors.
- **`driver.Run`** (see device driver below) drives the hardware at a fixed frame rate.
- **`sched.Run`** fires scheduled jobs (reminders/alarms/periodic).

Chat entry is `handleChat`: a **deterministic music-shortcut path** (`tryMusicShortcut` / `parseMusicPlay`) intercepts plain control phrases *before* the LLM so they work 100% even with backends that lack function-calling. Otherwise it routes to `handleChatWithTools` (function-calling loop) when the active model supports tools, else `handleChatStreaming`.

### Protocol is the single source of truth — `internal/protocol`
All frontend↔backend WebSocket messages are defined here. Two channels: JSON text frames wrapped in an `Envelope` (discriminated by `Type`; events in `events.go`, commands in `commands.go`), and compact binary frames for the 240×240 screen mirror (`frame.go`). **Any protocol change must be mirrored in all three places: `internal/protocol`, `web/src/protocol.ts`, and `docs/PROTOCOL.md`.**

### Single-owner device driver — `internal/device`
ElectronBot's `Sync` is one atomic "send image + joint angles, read back feedback" USB operation, so **exactly one loop owns the device**. The `Driver` ticks at a fixed FPS, pulling the current pose (written via `SetPose`) and the current frame (from a `display.Source`), Syncs them together, and broadcasts the same frame + joint feedback to the UI via callbacks. Choreography and manual jog only *update the desired pose* — they never touch the transport directly. This prevents image/pose desync from multiple Syncers.

### Hardware abstraction — `internal/robot`
`Transport` interface abstracts the robot (6 joints, 240×240 RGB888 screen). `robot.NewMock` is the no-hardware default; `internal/robot/electronbot` is the real USB transport via **purego + libusb (no cgo)**. `connectRobot` honors `robot: auto|mock|electronbot` config — `auto` tries the real device and silently falls back to Mock. Joint indices, limits, and `ClampAngle` live in `robot.go`.

### LLM abstraction — `internal/llm`
`Provider` interface unifies backends behind streaming `Chat` + non-streaming `Complete` + `SupportsTools`. `Router` holds any number of providers and switches the active one at runtime (config-driven, editable from the settings page). Implementations: `echo.go` (local fake fallback), `openai.go` (OpenAI-compatible / Ollama), `xiaozhi.go` (小智 — WebSocket backend with its own built-in TTS, exposed via the optional `AudioReplier` interface for streamed audio). `RunToolLoop` (`agent.go`) drives the multi-round function-calling loop.

### Display pipeline — `internal/display`
`Compositor` composes the screen image from layered sources: optional camera → offline emotion clips (`ClipSource`, hot-reloadable user uploads) → procedural animated face (blink/lip-sync fallback). Built-in default emotions are seeded once into `emotions/` next to `config.json`.

### Other modules
- `internal/tools` — function-calling tool registry; concrete tools (emotion, action, weather, reminder, music, vision/look, image-gen, music-gen) are registered in `buildTools` (`main.go`) with side effects injected as closures.
- `internal/choreography` — keyframe-interpolated 6-axis action sequences; built-ins + user recordings (`actions.json`), recorded live via the record commands.
- `internal/speech` / `internal/gesture` — sidecar clients (Mock when no `sidecar_url` configured). `internal/netspeech` — OpenAI-compatible network TTS/ASR. `internal/minimax` — MiniMax multimodal (image/music generation, TTS).
- `internal/music` — search + playback via `mpg123` subprocess (no cgo); sources are `kuwo` (default) or `qq` (`internal/music/qqmusic.go`, `qqlogin.go`).
- `internal/config` — JSON config load/persist; the settings page edits models/IO/device and writes back. API keys are stored in plaintext (self-hosted/single-machine assumption).
- `internal/server` — HTTP + WebSocket (hub/client/broadcast/heartbeat), serves the embedded SPA.

### Frontend — `web/`
Embedded via `go:embed public` (`web.go`) so the binary is a single-file distribution. **The frontend in `web/public/` is plain vanilla JS/HTML/CSS with no build step and no external deps** (`app.js`, `index.html`, `styles.css`) plus vendored three.js for the 3D model viewer. There is no `package.json`. Note: the README's mention of "Vue3 + Vite" is aspirational — the shipped frontend is the no-build version. `web/src/protocol.ts` is the TypeScript mirror of the protocol contract.

## Conventions

- Pure Go, `CGO_ENABLED=0` everywhere — native needs go through sidecars or purego, never cgo. Keep it that way.
- Runtime data (`config.json`, `actions.json`, `emotions/`, `jobs.json`) lives next to the config file and is gitignored, as are sidecar binaries/models/venvs.
- The fixed `toolRules` system prompt (`main.go`) is always appended after the user persona because some models (e.g. MiniMax-M3) only *describe* actions instead of calling tools unless explicitly forced.

## Docs

`docs/PROTOCOL.md` (message contract), `docs/ELECTRONBOT.md` (real-hardware setup), `docs/SPEECH.md`, `docs/MUSIC.md`, `docs/EMOTIONS.md`, `docs/TASKS.md`. Sidecar setup: `sidecars/speech/README.md`, `sidecars/gesture/README.md`.
