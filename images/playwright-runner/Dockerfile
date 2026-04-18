# Opinionated small Playwright image: node:22-slim + Chromium-only + pinned
# Playwright version. ~500MB (vs ~2GB for mcr.microsoft.com/playwright:*-noble).
FROM node:22-slim

ENV PLAYWRIGHT_BROWSERS_PATH=/ms-playwright \
    NODE_ENV=production \
    NODE_PATH=/app/node_modules

WORKDIR /app

COPY package.json ./
RUN npm install --omit=dev --no-fund --no-audit \
 && npx playwright install --with-deps chromium \
 && chmod -R a+rX /ms-playwright \
 && rm -rf /root/.npm

COPY playwright.config.js runner.js runner.test.js ./

RUN useradd --create-home --home-dir /home/runner --shell /usr/sbin/nologin runner \
 && chown -R runner:runner /app
USER runner

ENTRYPOINT ["node", "/app/runner.js"]
