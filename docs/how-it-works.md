# How boxr Works

A plain English breakdown of what happens when you run `boxr run /bin/sh`.

## Linux Namespaces

A namespace is a kernel feature that wraps a global resource and makes a process think it has its own private version of it.

boxr uses 4 namespaces:

| Namespace | Flag | What it isolates |
|---|---|---|
| PID | CLONE_NEWPID | Process IDs — container gets its own PID 1 |
| UTS | CLONE_NEWUTS | Hostname and domain name |
| Mount | CLONE_NEWNS | Filesystem mount points |
| IPC | CLONE_NEWIPC | Message queues, semaphores, shared memory |

When you call `clone()` or `exec()` with these flags, the kernel creates a new instance of each resource — invisible to everything outside.

## cgroups v2

Control groups limit how much of the host's resources a process can use.

boxr writes to `/sys/fs/cgroup/boxr/<pid>/` to:
- Cap memory: `memory.max`
- Cap CPU: `cpu.max`
- Assign the process: `cgroup.procs`

## What's Next

- **Network namespace** — give the container its own network stack, connect it to host via a `veth` pair
- **Overlay filesystem** — stack read-only image layers with a writable top layer, exactly like Docker
- **Registry API** — pull an Alpine rootfs from Docker Hub without Docker installed
