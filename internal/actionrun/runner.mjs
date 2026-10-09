import { pathToFileURL } from "node:url";

const actionDirectory = process.env.BEAM_ACTION_DIRECTORY;
const entrypoint = process.env.BEAM_ACTION_ENTRYPOINT;
const logLevel = process.env.BEAM_ACTION_LOG_LEVEL || "info";
const runtimeMode = process.env.BEAM_ACTION_RUNTIME || "mock";

if (!actionDirectory || !entrypoint) {
  throw new Error("The local action runner is missing its action directory or entrypoint.");
}

let encodedInput = "";
for await (const chunk of process.stdin) {
  encodedInput += chunk;
}
const payload = JSON.parse(encodedInput || "{}");
const state = {};
const artifacts = [];
const storageValues = new Map();
const abortController = new AbortController();

process.once("SIGINT", () => abortController.abort());
process.once("SIGTERM", () => abortController.abort());

const logger = localLogger(logLevel);
const context = {
  workflowRunId: "local_workflow_run",
  stepRunId: "local_step_run",
  stepId: payload.packageName || "local_action",
  attempt: 1,
  logger,
  state: {
    get: () => ({ ...state }),
    set: (nextState) => {
      for (const key of Object.keys(state)) {
        delete state[key];
      }
      Object.assign(state, nextState || {});
    },
    patch: (partialState) => Object.assign(state, partialState || {}),
  },
  storage: {
    getJson: async (key) => storageValues.get(key),
    putJson: async (key, value) => {
      storageValues.set(key, value);
    },
  },
  artifacts: {
    publish: async (artifact) => {
      const published = {
        ...artifact,
        uri: stringValue(artifact?.uri) || `memory://artifacts/${artifacts.length + 1}`,
      };
      artifacts.push(published);
      return published;
    },
  },
  secrets: {
    get: async (name) => secretValue(payload.secrets || {}, name),
  },
  beam: runtimeMode === "mock" ? { mock: true } : {},
  signal: abortController.signal,
};

const moduleUrl = `${pathToFileURL(entrypoint).href}?t=${Date.now()}`;
const actionModule = await import(moduleUrl);
const execute =
  typeof actionModule.execute === "function"
    ? actionModule.execute
    : typeof actionModule.default === "function"
      ? actionModule.default
      : actionModule.default && typeof actionModule.default.execute === "function"
        ? actionModule.default.execute
        : null;

if (!execute) {
  throw new Error(`Entrypoint ${entrypoint} does not export execute.`);
}

const result = await execute(
  {
    config: payload.config || {},
    inputs: payload.inputs || {},
  },
  context,
);

process.stdout.write(
  `${JSON.stringify(
    {
      ...(result || {}),
      state: result?.state || state,
      artifacts: result?.artifacts || artifacts,
    },
    null,
    2,
  )}\n`,
);

function localLogger(level) {
  const order = ["debug", "info", "warn", "error"];
  const minimum = order.includes(level) ? order.indexOf(level) : 1;
  const enabled = (candidate) => order.indexOf(candidate) >= minimum;
  const write = (candidate, message, details) => {
    if (!enabled(candidate)) {
      return;
    }
    const suffix = details === undefined ? "" : ` ${JSON.stringify(details)}`;
    process.stderr.write(`[${candidate}] ${message}${suffix}\n`);
  };
  return {
    debug: (message, details) => write("debug", message, details),
    info: (message, details) => write("info", message, details),
    warn: (message, details) => write("warn", message, details),
    error: (message, details) => write("error", message, details),
  };
}

function secretValue(secrets, name) {
  const value = secrets[name];
  if (value === undefined || value === null) {
    return null;
  }
  return typeof value === "string" ? value : JSON.stringify(value);
}

function stringValue(value) {
  return typeof value === "string" && value.trim() ? value.trim() : null;
}
