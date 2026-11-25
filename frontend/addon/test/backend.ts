import type { Deployment, Key, Me, Model, Node } from '../src/types';

export interface RecordedCall {
  method: string;
  path: string;
  body: unknown;
}

type Handler = (call: RecordedCall) => unknown;

const json = (body: unknown, status = 200) =>
  new Response(status === 204 ? null : JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });

/**
 * A fake backend for `renderInHost({ addonFetch })`, routes keyed `METHOD /v1/path`. Unmocked
 * routes answer 404 in the backend's error envelope, so a page that calls more than a test
 * expects shows up as an error instead of hanging.
 */
export function createBackend(routes: Record<string, Handler>) {
  const calls: RecordedCall[] = [];
  const addonFetch = async (request: Request): Promise<Response> => {
    const path = new URL(request.url).pathname.replace(/^\/addon-assets\/[^/]+\/api/, '');
    const text = request.method === 'GET' ? '' : await request.clone().text();
    const call = { method: request.method, path, body: text ? (JSON.parse(text) as unknown) : undefined };
    calls.push(call);
    const handler = routes[`${request.method} ${path}`];
    if (!handler) {
      return json({ error: { code: 'not_found', message: `no mock for ${request.method} ${path}` } }, 404);
    }
    const result = handler(call);
    return result instanceof Response ? result : json(result);
  };
  return { addonFetch, calls };
}

export const created = (body: unknown) => json(body, 201);

export const adminMe: Me = { name: 'admin', roles: ['Admin'], admin: true };
export const userMe: Me = { name: 'viewer', roles: [], admin: false };

export const model: Model = {
  id: 'm1',
  name: 'qwen-7b',
  reference: 'registry.example.com/models/qwen:7b',
  engine_image: '',
  engine_args: [],
  command: null,
  container_port: 8000,
  memory_mb: 16384,
  vgpus: 1,
  auto: true,
  labels: {},
  created_at: 1,
  updated_at: 1,
};

export const node = (overrides: Partial<Node> = {}): Node => ({
  cluster: 'edge',
  name: 'worker-1',
  ip: '10.0.0.1',
  architecture: 'arm64',
  active: true,
  vcpus: 8,
  memory_mb: 32768,
  vgpus: 1,
  vram_mb: 0,
  csi_drivers: [],
  modelpack_csi: true,
  ...overrides,
});

export const deployment: Deployment = {
  id: 'd1',
  model_id: 'm1',
  model_name: 'qwen-7b',
  cluster: 'edge',
  node: 'worker-1',
  state: 'failed',
  error: 'image pull failed',
  created_at: 1,
  updated_at: 1,
};

export const key: Key = { id: 'k1', name: 'editor', prefix: 'sk-abc', created_at: 1, last_used_at: null };
