#!/usr/bin/env python3
"""Run a herdr client for one named session in a pseudo-terminal that this script owns.

Usage: client.py <session> <control-file> <rows> <cols>

The client gets a clean environment (as the E11-S3 recipe). Each line "rows cols" that is
appended to the control file sets the terminal size with TIOCSWINSZ, so the client resizes
the panes of the session (bug #199). The line "quit" ends the client; the session stays.
"""
import fcntl, os, pty, select, signal, struct, sys, termios, time

session, control, rows, cols = sys.argv[1], sys.argv[2], int(sys.argv[3]), int(sys.argv[4])
home = os.environ["HOME"]
env = {
    "HOME": home,
    "PATH": f"{home}/.nvm/versions/node/v24.15.0/bin:{home}/.local/bin:/usr/bin:/bin:/usr/sbin:/sbin",
    "TERM": "xterm-256color",
    "LANG": "en_US.UTF-8",
}


def size(fd, r, c):
    fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", r, c, 0, 0))


pid, fd = pty.fork()
if pid == 0:
    size(sys.stdin.fileno(), rows, cols)
    os.execve(f"{home}/.local/bin/herdr", ["herdr", "--session", session], env)
size(fd, rows, cols)
open(control, "a").close()
done = 0
while True:
    r, _, _ = select.select([fd], [], [], 0.2)
    if r:
        try:
            os.read(fd, 65536)  # drain the screen; nobody looks at it
        except OSError:
            break
    with open(control) as f:
        lines = f.read().splitlines()
    for line in lines[done:]:
        if line == "quit":
            os.kill(pid, signal.SIGHUP)
            time.sleep(0.5)
            sys.exit(0)
        r2, c2 = map(int, line.split())
        size(fd, r2, c2)
        os.kill(pid, signal.SIGWINCH)
        print(f"{time.time():.3f} size {r2}x{c2}", flush=True)
    done = len(lines)
