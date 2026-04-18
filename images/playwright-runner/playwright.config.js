// Baked-in Playwright configuration. runner.js copies the user script from
// the /scripts ConfigMap mount into /tmp/spec before invoking Playwright —
// see the SCRIPT_STAGE_DIR comment in runner.js for why.
module.exports = {
  testDir: '/tmp/spec',
  testMatch: /.*\.spec\.js/,
  reporter: [['json']],
  forbidOnly: true,
  workers: 1,
  use: {
    headless: true,
    trace: 'off',
    video: 'off',
    screenshot: 'off',
  },
  projects: [
    { name: 'chromium', use: { browserName: 'chromium' } },
  ],
};
