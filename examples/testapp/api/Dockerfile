FROM node:20-alpine

WORKDIR /app

# Install deps first (layer cache — only re-run on package.json change).
COPY package.json ./
RUN npm install --omit=dev --no-audit --no-fund && npm cache clean --force

COPY index.js ./

# PORT is overridden in docker-compose via the PORT environment variable.
ENV PORT=3001
EXPOSE 3001

# Use tini as PID 1 so SIGTERM is forwarded correctly to the node process.
RUN apk add --no-cache tini
ENTRYPOINT ["/sbin/tini", "--"]
CMD ["node", "index.js"]
