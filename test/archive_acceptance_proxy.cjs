// Fixture-only loopback TLS/WS forwarding. No mocked application responses.
"use strict";
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const http = require("node:http");
const https = require("node:https");

const root = process.argv[2];
assert(root && /^\/tmp\/archive-acceptance-[a-z0-9]{1,12}$/.test(root));
assert.equal(fs.realpathSync(root), root);
const spec = JSON.parse(fs.readFileSync(path.join(root, "fixture.json"), "utf8"));
assert.equal(spec.root, root);
assert.equal(spec.purpose, "disposable-archive-acceptance");
assert.equal(spec.status, "prepared");
assert.equal(spec.uid, process.getuid());
assert.equal(spec.workspaceName, "acceptance-" + path.basename(root).slice("archive-acceptance-".length));
const socketPath = path.join(root, "run", spec.workspaceName, "portal.sock");
const peers = new Set();
const server = https.createServer({
  key: fs.readFileSync(path.join(root, "tls/key.pem")),
  cert: fs.readFileSync(path.join(root, "tls/cert.pem")),
}, (request, response) => {
  const upstream = http.request({socketPath, path: request.url,
    method: request.method, headers: request.headers}, incoming => {
    response.writeHead(incoming.statusCode, incoming.headers);
    incoming.pipe(response);
    incoming.on("error", () => response.destroy());
  });
  upstream.on("error", () => {
    if (response.headersSent) return response.destroy();
    response.writeHead(502, {"Content-Type": "text/plain"});
    response.end("Fixture portal unavailable\n");
  });
  request.on("aborted", () => upstream.destroy());
  response.on("close", () => upstream.destroy());
  request.pipe(upstream);
});
server.on("connection", peer => {
  peers.add(peer);
  peer.once("close", () => peers.delete(peer));
});
server.on("upgrade", (request, downstream, head) => {
  const upstream = http.request({socketPath, path: request.url,
    method: request.method, headers: request.headers});
  upstream.on("upgrade", (response, peer, extra) => {
    const headers = [];
    for (let i = 0; i < response.rawHeaders.length; i += 2)
      headers.push(response.rawHeaders[i] + ": " + response.rawHeaders[i + 1]);
    downstream.write(`HTTP/1.1 ${response.statusCode} ${response.statusMessage}\r\n${headers.join("\r\n")}\r\n\r\n`);
    if (extra.length) downstream.write(extra);
    if (head.length) peer.write(head);
    peer.on("error", () => downstream.destroy());
    downstream.on("error", () => peer.destroy());
    peer.on("close", () => downstream.destroy());
    downstream.on("close", () => peer.destroy());
    peer.pipe(downstream).pipe(peer);
  });
  upstream.on("response", response => { response.resume(); downstream.destroy(); });
  upstream.on("error", () => downstream.destroy());
  downstream.on("error", () => upstream.destroy());
  upstream.end();
});
server.listen(0, "127.0.0.1", () => {
  const metadata = {pid: process.pid, socketPath,
    origin: `https://127.0.0.1:${server.address().port}`};
  fs.writeFileSync(path.join(root, "proxy.json"), JSON.stringify(metadata) + "\n",
    {mode: 0o600, flag: "wx"});
  process.stdout.write(JSON.stringify(metadata) + "\n");
});
for (const signal of ["SIGINT", "SIGTERM"]) process.on(signal, () => {
  server.close();
  for (const peer of peers) peer.destroy();
});
