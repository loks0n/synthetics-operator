#!/usr/bin/env node
// playwright-runner is the entrypoint for the PlaywrightTest CronJob's main
// container. It invokes the Playwright test runner with the JSON reporter,
// converts the report into a TestResult, and writes it to /results/output.json
// for the test-sidecar to pick up and publish over NATS.

'use strict';

const { spawn } = require('node:child_process');
const fs = require('node:fs');

const OUTPUT_FILE = '/results/output.json';
const PLAYWRIGHT_BIN = '/app/node_modules/.bin/playwright';
const PLAYWRIGHT_CONFIG = '/app/playwright.config.js';
// The ConfigMap volume mounts scripts through kubelet's atomic-swap symlink
// at /scripts/..<timestamp>/test.spec.js. Playwright's JSON reporter resolves
// that symlink and embeds the timestamped path in the suite label, which
// would make suite a cardinality bomb that rotates every pod restart. Copy
// the spec into a stable path before invoking Playwright.
const SCRIPT_SRC = '/scripts/test.spec.js';
const SCRIPT_STAGE_DIR = '/tmp/spec';
const SCRIPT_STAGED = `${SCRIPT_STAGE_DIR}/test.spec.js`;

async function main() {
  const name = process.env.PLAYWRIGHT_TEST_NAME || '';
  const namespace = process.env.PLAYWRIGHT_TEST_NAMESPACE || '';
  const started = new Date();

  fs.mkdirSync(SCRIPT_STAGE_DIR, { recursive: true });
  fs.copyFileSync(SCRIPT_SRC, SCRIPT_STAGED);

  const { stdout, exitCode } = await runPlaywright();
  const durationMs = Date.now() - started.getTime();

  let tests = [];
  try {
    tests = parseReport(JSON.parse(stdout));
  } catch (err) {
    console.error(`playwright-runner: failed to parse JSON report: ${err.message}`);
  }

  const result = {
    kind: 'PlaywrightTest',
    name,
    namespace,
    success: exitCode === 0,
    timestamp: started.toISOString(),
    durationMs,
    tests,
  };

  fs.writeFileSync(OUTPUT_FILE, JSON.stringify(result));
  process.exit(exitCode);
}

function runPlaywright() {
  return new Promise((resolve) => {
    const proc = spawn(
      PLAYWRIGHT_BIN,
      ['test', '--config', PLAYWRIGHT_CONFIG],
      { stdio: ['ignore', 'pipe', 'inherit'] },
    );
    let stdout = '';
    proc.stdout.on('data', (chunk) => {
      stdout += chunk.toString();
    });
    proc.on('close', (code) => resolve({ stdout, exitCode: code ?? 1 }));
  });
}

// parseReport walks Playwright's JSON reporter output and flattens it into a
// [{suite, test, passed, durationMs}] list. A "suite" path joins nested suite
// titles with " > " so filenames and describe() blocks both contribute.
function parseReport(report) {
  const out = [];
  const walk = (suites, trail) => {
    for (const suite of suites || []) {
      const nextTrail = suite.title ? [...trail, suite.title] : trail;
      const suitePath = nextTrail.join(' > ');
      for (const spec of suite.specs || []) {
        for (const test of spec.tests || []) {
          const results = test.results || [];
          const last = results[results.length - 1];
          if (!last) continue;
          out.push({
            suite: suitePath,
            test: spec.title,
            passed: last.status === 'passed',
            durationMs: Math.round(last.duration || 0),
          });
        }
      }
      walk(suite.suites, nextTrail);
    }
  };
  walk(report.suites || [], []);
  return out;
}

module.exports = { parseReport };

if (require.main === module) {
  main().catch((err) => {
    console.error(err);
    process.exit(1);
  });
}
