'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');

const { parseReport } = require('./runner.js');

test('flattens nested suites into suite > subsuite paths', () => {
  const report = {
    suites: [
      {
        title: 'checkout.spec.js',
        specs: [
          {
            title: 'root-level spec',
            tests: [{ results: [{ status: 'passed', duration: 123 }] }],
          },
        ],
        suites: [
          {
            title: 'when logged in',
            specs: [
              {
                title: 'adds item to cart',
                tests: [{ results: [{ status: 'passed', duration: 45.6 }] }],
              },
              {
                title: 'checks out',
                tests: [{ results: [{ status: 'failed', duration: 200 }] }],
              },
            ],
          },
        ],
      },
    ],
  };

  const tests = parseReport(report);
  assert.deepEqual(tests, [
    { suite: 'checkout.spec.js', test: 'root-level spec', passed: true, durationMs: 123 },
    { suite: 'checkout.spec.js > when logged in', test: 'adds item to cart', passed: true, durationMs: 46 },
    { suite: 'checkout.spec.js > when logged in', test: 'checks out', passed: false, durationMs: 200 },
  ]);
});

test('uses the last result on a test (retries)', () => {
  const report = {
    suites: [{
      title: 'flaky.spec.js',
      specs: [{
        title: 'eventually passes',
        tests: [{
          results: [
            { status: 'failed', duration: 100 },
            { status: 'passed', duration: 80 },
          ],
        }],
      }],
    }],
  };

  assert.deepEqual(parseReport(report), [
    { suite: 'flaky.spec.js', test: 'eventually passes', passed: true, durationMs: 80 },
  ]);
});

test('returns empty list for missing suites', () => {
  assert.deepEqual(parseReport({}), []);
  assert.deepEqual(parseReport({ suites: [] }), []);
});

test('skips specs with no results', () => {
  const report = {
    suites: [{
      title: 's.spec.js',
      specs: [
        { title: 'no results', tests: [{ results: [] }] },
        { title: 'has result', tests: [{ results: [{ status: 'passed', duration: 10 }] }] },
      ],
    }],
  };

  assert.deepEqual(parseReport(report), [
    { suite: 's.spec.js', test: 'has result', passed: true, durationMs: 10 },
  ]);
});
