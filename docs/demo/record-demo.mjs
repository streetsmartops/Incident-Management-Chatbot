// Records the incidentd demo walkthrough against the local demo console.
//
//   go run ./cmd/demo -listen 127.0.0.1:8090 &
//   node docs/demo/record-demo.mjs http://127.0.0.1:8090 docs/demo
//
// Produces <out>/incidentd-demo.webm and screenshots <out>/../images/demo-*.png.
import { createRequire } from 'module';
import fs from 'fs';
import path from 'path';

const require = createRequire(import.meta.url);
const { chromium } = require('playwright');

const base = process.argv[2] || 'http://127.0.0.1:8090';
const out = process.argv[3] || 'docs/demo';
const imgDir = path.join(out, '..', 'images');
fs.mkdirSync(out, { recursive: true });
fs.mkdirSync(imgDir, { recursive: true });

const browser = await chromium.launch();
const context = await browser.newContext({
  viewport: { width: 1440, height: 860 },
  recordVideo: { dir: out, size: { width: 1440, height: 860 } },
});
const page = await context.newPage();
const pause = (ms) => page.waitForTimeout(ms);
const caption = async (text, ms = 2600) => { await page.evaluate((t) => window.caption(t), text); await pause(ms); };
const as = async (user) => { await page.selectOption('#user', user); await pause(400); };
const say = async (text, ms = 1500) => {
  await page.fill('#input', '');
  await page.type('#input', text, { delay: 18 });
  await pause(300);
  await page.press('#input', 'Enter');
  await pause(ms);
};
const waitText = (text, timeout = 20000) => page.locator('#messages').getByText(text, { exact: false }).last().waitFor({ timeout });
const shot = async (name) => {
  await page.evaluate(() => window.caption(''));
  await pause(150);
  await page.screenshot({ path: path.join(imgDir, `demo-${name}.png`) });
};

await page.goto(base);
await pause(800);
await caption('incidentd demo: SLO breach → auto-declare → AI triage → approved rollback → postmortem', 3500);
await caption('Real incident core, poller, triage, remediation and postmortem engines. Vendors are in-memory fakes; the LLM is scripted.', 3500);

// 1. SLO breach -> proposal -> auto-declare
await caption('Multiplexer reports the enterprise tenant "acme" breaching its checkout SLO');
await page.click('#breach');
await waitText('Proposed SEV1');
await caption('First evaluation: the bot proposes SEV1 (the breach must persist for 2 evaluations to auto-declare)', 3200);
await waitText('Auto-declared');
await caption('Second evaluation: SEV1 auto-declared. War room created, PagerDuty paged, Jira ticket filed (see Integration calls)', 3800);
await shot('1-auto-declared');

// 2. War room, IC
await page.evaluate(() => window.selectChannel('inc-'));
await pause(1200);
await caption('In the war room, Alice takes the vacant IC role');
await as('alice');
await say('/incident role ic me');
await say('/incident status investigating');

// 3. AI triage
await caption('Alice runs AI triage: read-only tools, cited evidence, catalog-only recommendations');
await say('/incident triage', 800);
await waitText('AI triage', 30000);
await pause(800);
await caption('Uncited hypotheses and non-catalog actions are dropped in code; secrets and emails in logs were redacted before the model saw them', 4200);
await page.locator('#messages').evaluate((el) => { el.scrollTop = el.scrollHeight; });
await shot('2-triage');

// 4. Remediation with two-person approval
await caption('Bob (on-call) requests the suggested rollback: a dry-run diff and an approval card. Nothing runs yet', 3200);
await as('bob');
await page.getByRole('button', { name: /Request k8s.rollout_undo/ }).last().click();
await waitText('Approval needed');
await pause(1200);
await shot('3-approval');
await caption('High risk: needs the IC and the service owner, two different people, neither of them the requester', 3200);
await page.getByRole('button', { name: 'Approve' }).last().click();
await waitText('requester cannot approve');
await caption('Bob tries to approve his own request: refused', 2600);
await as('alice');
await page.getByRole('button', { name: 'Approve' }).last().click();
await waitText('Still needed');
await caption('Alice approves as IC. One more approval needed: the service owner', 2800);
await as('carol');
await page.getByRole('button', { name: 'Approve' }).last().click();
await waitText('Executor is disabled');
await caption('Both approvals recorded in the timeline. The executor kill switch is off in this phase, so it reports dry-run only', 4200);
await shot('4-approved-dry-run');

// 5. Mitigate, update, resolve
await as('alice');
await caption('The team rolls back manually, then Alice walks the incident to resolved');
await say('/incident status mitigating');
await say('/incident update "Rolled back checkout-api to v2.3.0; error rate recovering"', 1800);
await say('/incident status monitoring');
await say('/incident resolve "Bad deploy v2.3.1 rolled back"', 1800);

// 6. Postmortem + timeline
await caption('Postmortem draft: timings, error budget burned, blameless narrative, action items');
await say('/incident postmortem draft', 800);
await waitText('Postmortem draft', 20000);
await pause(1500);
await page.locator('#messages').evaluate((el) => { el.scrollTop = el.scrollHeight; });
await shot('5-postmortem');
await caption('Every step is in a hash-chained, append-only timeline', 2200);
await say('/incident timeline --last 12', 2500);
await shot('6-timeline');
await caption('incidentd: declare, coordinate, triage, remediate with approval, learn. Docs: docs/README.md', 3500);
await page.evaluate(() => window.caption(''));
await pause(500);

const video = page.video();
await context.close();
await browser.close();
const src = await video.path();
const dst = path.join(out, 'incidentd-demo.webm');
fs.renameSync(src, dst);
console.log('video:', dst);
