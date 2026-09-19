// Load scenario for the wager service. Run with `make load` (k6 in Docker,
// host network) against the three compose instances.
//
// Traffic mix per iteration, chosen per VU with a stable wallet so that
// contention on the same wallet row is part of the test:
//   60% BET      (1.00 .. 5.00)
//   20% WIN      (1.00 .. 5.00)
//    5% LOSS     (0.00)
//    5% REFUND   of this VU's last processed bet (may be REJECTED if already reversed)
//   10% replay   of this VU's last bet with the same idempotency key
//
// Expected non-2xx outcomes (422 rejections, 409 conflicts) are counted as
// business results, not failures; only 5xx and transport errors fail.
import http from "k6/http";
import { check, sleep } from "k6";
import { Counter, Trend, Gauge } from "k6/metrics";

const APIS = (__ENV.APIS || "http://localhost:8081,http://localhost:8082,http://localhost:8083").split(",");
const KC_URL = __ENV.KC_URL || "http://localhost:8080";
const WALLETS = Number(__ENV.WALLETS || 40);
const INITIAL_BALANCE = __ENV.INITIAL_BALANCE || "100000.00";

export const options = {
  scenarios: {
    providers: {
      executor: "ramping-vus",
      exec: "provider",
      startVUs: 0,
      stages: [
        { duration: __ENV.RAMP || "20s", target: Number(__ENV.VUS || 60) },
        { duration: __ENV.HOLD || "60s", target: Number(__ENV.VUS || 60) },
        { duration: "10s", target: 0 },
      ],
      gracefulRampDown: "10s",
    },
    outbox: {
      executor: "constant-vus",
      exec: "probeOutbox",
      vus: 1,
      duration: __ENV.PROBE || "95s",
    },
  },
  thresholds: {
    http_req_failed: ["rate<0.01"],
    "http_req_duration{endpoint:submit}": ["p(95)<500", "p(99)<1000"],
    outbox_lag_seconds: ["value<10"],
  },
  summaryTrendStats: ["avg", "min", "med", "p(50)", "p(90)", "p(95)", "p(99)", "max"],
};

http.setResponseCallback(http.expectedStatuses(200, 201, 202, 409, 422));

const processed = new Counter("wager_processed");
const rejected = new Counter("wager_rejected");
const replays = new Counter("wager_replays");
const pending = new Counter("wager_pending_reference");
const conflicts = new Counter("wager_conflicts");
const outboxLag = new Gauge("outbox_lag_seconds");
const submitLatency = new Trend("submit_latency_ms", true);

function token(clientId) {
  const res = http.post(
    `${KC_URL}/realms/wager/protocol/openid-connect/token`,
    { grant_type: "client_credentials", client_id: clientId, client_secret: `${clientId}-secret` },
    { tags: { endpoint: "token" } },
  );
  if (res.status !== 200) throw new Error(`token ${clientId}: ${res.status} ${res.body}`);
  return res.json("access_token");
}

function uuid() {
  return "xxxxxxxx-xxxx-4xxx-yxxx-xxxxxxxxxxxx".replace(/[xy]/g, (c) => {
    const r = (Math.random() * 16) | 0;
    return (c === "x" ? r : (r & 0x3) | 0x8).toString(16);
  });
}

export function setup() {
  const internal = token("wallet-internal");
  const provider = token("provider-a");
  const wallets = [];
  for (let i = 0; i < WALLETS; i++) {
    const playerId = uuid();
    const res = http.post(
      `${APIS[i % APIS.length]}/wallets`,
      JSON.stringify({ playerId, initialBalance: { amount: INITIAL_BALANCE, currency: "BRL" } }),
      { headers: { "Content-Type": "application/json", Authorization: `Bearer ${internal}` }, tags: { endpoint: "open" } },
    );
    if (res.status !== 201) throw new Error(`open wallet: ${res.status} ${res.body}`);
    wallets.push({ id: res.json("id"), playerId });
  }
  return { provider, internal, wallets, run: uuid().slice(0, 8) };
}

function amount() {
  return (1 + Math.floor(Math.random() * 400) / 100).toFixed(2);
}

export function provider(data) {
  const wallet = data.wallets[__VU % data.wallets.length];
  const api = APIS[__ITER % APIS.length];
  const roll = Math.random();
  let op;
  if (roll < 0.6) {
    op = { kind: "BET", amount: amount(), external: `${data.run}-v${__VU}-bet-${__ITER}` };
  } else if (roll < 0.8) {
    op = { kind: "WIN", amount: amount(), external: `${data.run}-v${__VU}-win-${__ITER}` };
  } else if (roll < 0.85) {
    op = { kind: "LOSS", amount: "0.00", external: `${data.run}-v${__VU}-loss-${__ITER}` };
  } else if (roll < 0.9 && provider.lastBet) {
    op = { kind: "REFUND", amount: provider.lastBet.amount, external: `${data.run}-v${__VU}-refund-${__ITER}`, reference: provider.lastBet.external };
  } else if (provider.lastBet) {
    op = { ...provider.lastBet, replay: true };
  } else {
    op = { kind: "BET", amount: amount(), external: `${data.run}-v${__VU}-bet-${__ITER}` };
  }

  const body = {
    providerId: "provider-a",
    externalTransactionId: op.external,
    playerId: wallet.playerId,
    walletId: wallet.id,
    roundId: `round-${__VU}`,
    gameId: "fortune-chimp",
    kind: op.kind,
    money: { amount: op.amount, currency: "BRL" },
  };
  if (op.reference) body.referenceExternalTransactionId = op.reference;

  const res = http.post(`${api}/wagering/transactions`, JSON.stringify(body), {
    headers: {
      "Content-Type": "application/json",
      Authorization: `Bearer ${data.provider}`,
      "Idempotency-Key": `provider-a:${op.external}`,
    },
    tags: { endpoint: "submit", kind: op.kind },
  });
  submitLatency.add(res.timings.duration);

  check(res, { "no 5xx": (r) => r.status < 500 });
  if (res.status === 200) {
    if (res.json("idempotentReplay")) replays.add(1);
    else processed.add(1);
    if (op.kind === "BET" && !op.replay) provider.lastBet = { kind: "BET", amount: op.amount, external: op.external };
  } else if (res.status === 422) {
    rejected.add(1);
  } else if (res.status === 202) {
    pending.add(1);
  } else if (res.status === 409) {
    conflicts.add(1);
  }
  sleep(0.05);
}

// probeOutbox samples the outbox lag gauge of each instance every 2s.
export function probeOutbox() {
  for (const api of APIS) {
    const res = http.get(`${api}/metrics`, { tags: { endpoint: "metrics" } });
    const m = /^outbox_lag_seconds (\S+)/m.exec(res.body || "");
    if (m) outboxLag.add(Number(m[1]));
  }
  sleep(2);
}

export function teardown(data) {
  let consistent = 0;
  for (const w of data.wallets) {
    const res = http.post(`${APIS[0]}/wallets/${w.id}/reconciliation`, null, {
      headers: { Authorization: `Bearer ${data.internal}` },
      tags: { endpoint: "reconcile" },
    });
    if (res.status === 200 && res.json("consistent") === true) consistent++;
  }
  console.log(`reconciliation: ${consistent}/${data.wallets.length} wallets consistent`);
  if (consistent !== data.wallets.length) throw new Error("reconciliation found divergent wallets");
}
