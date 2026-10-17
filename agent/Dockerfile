# The Pi Durable agent. Node runs the TypeScript directly (type stripping);
# node:sqlite is built in, so there is nothing to compile.
FROM node:25-slim
WORKDIR /app
ENV NODE_ENV=production
COPY package.json package-lock.json ./
RUN npm ci --omit=dev --no-audit --no-fund
COPY src/ ./src/
USER node
EXPOSE 8082
CMD ["node", "--disable-warning=ExperimentalWarning", "src/main.ts"]
