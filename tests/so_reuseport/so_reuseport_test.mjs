import assert from 'assert/strict';
import http from 'http';
import process from 'process';

const ports = JSON.parse(process.env.ASSIGNED_PORTS)

for (const portName of [
  "@@//so_reuseport:reuseport_service",
  "@@//so_reuseport:reuseport_service.named_port1",
]) {
  const port = ports[portName];
  assert(port);

  const server = http.createServer(() => {});
  server.on('error', (err) => {
    assert.equal(err.code, 'EADDRINUSE');
  });
  server.listen({
    host: '127.0.0.1',
    port: parseInt(port, 10),
  });
}
