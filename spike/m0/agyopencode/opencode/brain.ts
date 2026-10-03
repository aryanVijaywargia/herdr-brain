// Brain shim prototype for OpenCode v2 (E16-S2 spike, HLD §8.2 and §8.5). Not the E16-S13 shim.
// A V2 server plugin: a plain default object with `id` and `setup`, so it needs no npm package.
// - `session.created` (from ctx.event.subscribe) starts `brain hook --profile opencode --event start`
//   with the event on stdin.
// - ctx.session.hook("context") is the V2 form of `experimental.chat.system.transform`: it adds the
//   printed brief to `event.system` of each model call of that session.
// - ctx.tool.hook("execute.after") is the V2 form of `tool.execute.after`: it logs the payload and
//   passes it to `brain hook --event tool`.
// Each call sends brain one shape: {session_id, cwd, event}, with the raw OpenCode event in `event`.
// The shim adds nothing when brain fails or prints nothing, and stops waiting after 5 s (E8-S2).
// Spike only: it logs to /private/tmp/e16s2/logs/opencode-shim.jsonl.
import { appendFileSync } from "node:fs"
import { spawn } from "node:child_process"

const LOG = "/private/tmp/e16s2/logs/opencode-shim.jsonl"
const TIMEOUT_MS = 5000

function safe(value: unknown): unknown {
  try {
    return JSON.parse(JSON.stringify(value, (_k, v) => (typeof v === "string" && v.length > 600 ? v.slice(0, 600) + "…" : v)))
  } catch (e) {
    return `unserializable: ${String(e)}`
  }
}

function log(rec: Record<string, unknown>) {
  try {
    appendFileSync(LOG, JSON.stringify({ t: Date.now(), ...rec }) + "\n")
  } catch {}
}

function herdrEnv() {
  return Object.fromEntries(Object.entries(process.env).filter(([k]) => k.startsWith("HERDR_")))
}

function runBrain(event: string, input: unknown): Promise<string> {
  return new Promise((resolve) => {
    let out = ""
    let done = false
    const finish = (s: string) => {
      if (!done) {
        done = true
        resolve(s)
      }
    }
    let child
    try {
      child = spawn("brain", ["hook", "--profile", "opencode", "--event", event], { stdio: ["pipe", "pipe", "ignore"] })
    } catch (e) {
      log({ kind: "brain-error", event, error: String(e) })
      return finish("")
    }
    const timer = setTimeout(() => {
      child.kill()
      log({ kind: "brain-timeout", event })
      finish("")
    }, TIMEOUT_MS)
    child.stdout.on("data", (d: Buffer) => (out += d))
    child.on("error", (e: Error) => {
      clearTimeout(timer)
      log({ kind: "brain-error", event, error: String(e) })
      finish("")
    })
    child.on("close", (code: number) => {
      clearTimeout(timer)
      finish(code === 0 ? out : "")
    })
    child.stdin.on("error", () => {})
    child.stdin.end(JSON.stringify(input))
  })
}

export default {
  id: "brain-spike",
  async setup(ctx: any) {
    log({ kind: "setup", version: ctx.app?.version, location: safe(ctx.location), pid: process.pid, ppid: process.ppid, herdr_env: herdrEnv() })
    const briefs = new Map<string, Promise<string>>()
    const input = (sessionID: string, cwd: string | undefined, event: unknown) => ({
      session_id: sessionID,
      cwd: cwd ?? ctx.location?.directory,
      event,
    })
    const controller = new AbortController()
    ;(async () => {
      for await (const ev of ctx.event.subscribe({ signal: controller.signal })) {
        if (typeof ev?.type === "string" && ev.type.startsWith("session.") && !ev.type.startsWith("session.message")) {
          log({ kind: "event", type: ev.type, event: safe(ev) })
        } else {
          log({ kind: "event", type: ev?.type })
        }
        if (ev?.type === "session.created") {
          const sessionID = ev.data?.sessionID ?? ev.sessionID
          if (typeof sessionID === "string") briefs.set(sessionID, runBrain("start", input(sessionID, ev.data?.location?.directory, ev)))
        }
      }
    })().catch((e) => log({ kind: "subscribe-error", error: String(e) }))

    await ctx.session.hook("context", async (ev: any) => {
      let pending = briefs.get(ev.sessionID)
      log({ kind: "context", sessionID: ev.sessionID, agent: ev.agent, model: safe(ev.model), system_parts: ev.system?.length, had_created: !!pending })
      if (!pending) {
        // No session.created in this server (a reopened session): start from the context hook.
        pending = runBrain("start", input(ev.sessionID, undefined, { type: "context", sessionID: ev.sessionID }))
        briefs.set(ev.sessionID, pending)
      }
      const brief = (await pending).trim()
      if (brief) ev.system.push({ type: "text", text: brief })
      log({ kind: "context-done", sessionID: ev.sessionID, added: brief.length, system_parts: ev.system?.length })
    })

    await ctx.tool.hook("execute.after", async (ev: any) => {
      log({ kind: "tool.after", event: safe(ev) })
      await runBrain("tool", input(ev.sessionID, undefined, ev))
    })

    return () => controller.abort()
  },
}
