cat > README.md << 'EOF'
<div align="center">

# 📦 boxr

**A mini Docker engine built from scratch using Linux namespaces + cgroups.**

No Docker. No containerd. Just raw syscalls.

![Go](https://img.shields.io/badge/Go-1.23-blue?style=flat-square&logo=go)
![License](https://img.shields.io/badge/license-MIT-green?style=flat-square)
![Platform](https://img.shields.io/badge/platform-Linux-orange?style=flat-square)
![Stars](https://img.shields.io/github/stars/manit6752025/boxr?style=flat-square)

</div>

---

## What is boxr?

boxr is a lightweight container engine that does what Docker does under the hood — isolate processes using Linux namespaces and cgroups — but with zero dependencies and clean, readable code.

Built to learn. Built to use. Built to be understood.

---

## Features

- ✅ PID namespace isolation — container gets its own PID 1
- ✅ UTS namespace — isolated hostname
- ✅ Mount namespace — private filesystem view
- ✅ IPC namespace — isolated interprocess communication
- ✅ cgroup v2 — CPU and memory limits
- 🔜 Network namespace + veth pair
- 🔜 Overlay filesystem (image layers)
- 🔜 Pull rootfs from Docker Hub registry API

---

## Quick Start

```bash
# Linux only (requires root for namespace creation)
git clone https://github.com/manit6752025/boxr
cd boxr
go build -o boxr ./cmd/boxr
sudo ./boxr run /bin/sh
```

You're now inside an isolated container. No Docker installed.

---

## How It Works

When you run `boxr run /bin/sh`, boxr:

1. Forks a new process with `CLONE_NEWPID` — giving it PID 1
2. Adds `CLONE_NEWUTS` — isolated hostname
3. Adds `CLONE_NEWNS` — private mount namespace
4. Adds `CLONE_NEWIPC` — isolated IPC
5. Applies cgroup v2 limits for CPU and memory
6. Exec's your command inside that isolated environment

No layers. No images. No daemon. Just a process that thinks it's alone.

---

## Project Structure
boxr/
├── cmd/boxr/main.go # CLI entry point
├── internal/
│ ├── container/
│ │ ├── run.go # Namespace isolation
│ │ └── cgroup.go # cgroup v2 limits
│ ├── image/ # (coming soon) registry pull
│ └── fs/ # (coming soon) overlay filesystem
├── docs/
│ └── how-it-works.md # Deep dive explanation
└── .github/workflows/release.yml # Cross-platform release pipeline


---

## Why Not Just Use Docker?

You should use Docker in production. boxr exists to show you what's underneath — and to be a real, usable tool that doesn't need a 500MB daemon running to isolate a process.

---

## Roadmap

- [x] v0.1 — Namespace + cgroup isolation, cross-platform releases
- [ ] v0.2 — Network namespace, veth pair, internet inside container
- [ ] v0.3 — Overlay filesystem, image layers
- [ ] v0.4 — Pull and run Alpine from Docker Hub with no Docker

---

## Contributing

PRs welcome. Open an issue first for big changes.

---

## License

MIT — free to use, modify, sell, build on.

---

<div align="center">
Built with 🔥 by <a href="https://github.com/manit6752025">manit6752025</a>
</div>
