import { renderInHost } from '@oakestra/dashboard-sdk/testing';
import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { ADDON_ID } from 'virtual:oakestra-addon';
import { describe, expect, it } from 'vitest';

import {
  adminMe,
  createBackend,
  created,
  deployment,
  key,
  model,
  node,
  userMe,
} from '../../test/backend';
import { assign, capacity, dropBlocker, EMPTY_PLAN, prune, slotsOn } from '../placement-plan';
import type { Deployment, Model } from '../types';
import { KeysPage } from './KeysPage';
import { ModelsPage } from './ModelsPage';
import { OverviewPage } from './OverviewPage';
import { PlacementPage } from './PlacementPage';
import { SettingsPage } from './SettingsPage';

const render = (ui: React.ReactElement, addonFetch: (r: Request) => Promise<Response>) =>
  renderInHost(ui, { addon: { addonId: ADDON_ID }, addonFetch });

describe('OverviewPage', () => {
  it('shows the endpoint, a curl snippet and the counts', async () => {
    const { addonFetch } = createBackend({
      'GET /v1/overview': () => ({
        public_url: 'https://llm.example.com/',
        version: '1.2.3',
        oakestra: { reachable: true },
        models: 3,
        deployments: { total: 4, ready: 2 },
        keys: 5,
        scheduler: 'least-loaded',
      }),
    });
    render(<OverviewPage />, addonFetch);
    expect(await screen.findByText('https://llm.example.com/v1')).toBeTruthy();
    expect(screen.getByText('2 of 4')).toBeTruthy();
    expect(screen.getByText('least-loaded')).toBeTruthy();
    expect(screen.getByText(/"model": "auto"/)).toBeTruthy();
    expect(screen.getByText(/llm\.example\.com\/v1\/chat\/completions/)).toBeTruthy();
  });

  it('switches the snippet to the OpenAI SDKs', async () => {
    const { addonFetch } = createBackend({
      'GET /v1/overview': () => ({
        public_url: 'https://llm.example.com/',
        version: '1.2.3',
        oakestra: { reachable: true },
        models: 1,
        deployments: { total: 1, ready: 1 },
        keys: 1,
        scheduler: 'least-inflight',
      }),
    });
    render(<OverviewPage />, addonFetch);
    const user = userEvent.setup();
    await user.click(await screen.findByRole('tab', { name: 'Python' }));
    expect(screen.getByText(/base_url="https:\/\/llm\.example\.com\/v1"/)).toBeTruthy();
    await user.click(screen.getByRole('tab', { name: 'JavaScript' }));
    expect(screen.getByText(/baseURL: "https:\/\/llm\.example\.com\/v1"/)).toBeTruthy();
  });

  it('points a new admin at the first missing step', async () => {
    const { addonFetch } = createBackend({
      'GET /v1/me': () => adminMe,
      'GET /v1/overview': () => ({
        public_url: 'https://llm.example.com/',
        version: '1.2.3',
        oakestra: { reachable: true },
        models: 0,
        deployments: { total: 0, ready: 0 },
        keys: 0,
        scheduler: 'least-inflight',
      }),
    });
    render(<OverviewPage />, addonFetch);
    expect(await screen.findByText('Add your first model')).toBeTruthy();
    expect(screen.getByRole('link', { name: /Go to Models/ })).toBeTruthy();
  });
});

describe('ModelsPage', () => {
  it('lists models with their deployment count and creates one', async () => {
    const backend = createBackend({
      'GET /v1/me': () => adminMe,
      'GET /v1/models': () => ({ models: [model] }),
      'GET /v1/deployments': () => ({ deployments: [deployment] }),
      'POST /v1/models': () => created(model),
    });
    render(<ModelsPage />, backend.addonFetch);
    expect(await screen.findByText('qwen-7b')).toBeTruthy();
    expect(screen.getByText('16 GB')).toBeTruthy();
    await waitFor(() => expect(screen.getByRole('cell', { name: '1' })).toBeTruthy());

    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: /add model/i }));
    // The engine settings stay folded away when creating.
    expect(screen.queryByLabelText('Extra engine arguments')).toBeNull();

    await user.click(screen.getByRole('button', { name: 'Create model' }));
    expect(screen.getByText('Enter a name.')).toBeTruthy();
    expect(screen.getByText('Enter the ModelPack reference.')).toBeTruthy();
    expect(backend.calls.some((c) => c.method === 'POST')).toBe(false);

    await user.type(screen.getByLabelText('Name'), 'llama');
    await user.type(screen.getByLabelText('ModelPack reference'), 'reg/llama:1');
    await user.type(screen.getByLabelText('Memory'), '7.5');
    expect(screen.getByText(/Requests 7680 MB/)).toBeTruthy();
    await user.click(screen.getByRole('button', { name: 'Create model' }));

    await waitFor(() => expect(backend.calls.some((c) => c.method === 'POST')).toBe(true));
    const post = backend.calls.find((c) => c.method === 'POST');
    expect(post?.body).toMatchObject({
      name: 'llama',
      reference: 'reg/llama:1',
      engine_image: '',
      engine_args: [],
      command: null,
      memory_mb: 7680,
      vgpus: 0,
      auto: true,
    });
  });

  it('refuses the reserved name auto before calling the backend', async () => {
    const backend = createBackend({
      'GET /v1/me': () => adminMe,
      'GET /v1/models': () => ({ models: [model] }),
      'GET /v1/deployments': () => ({ deployments: [] }),
    });
    render(<ModelsPage />, backend.addonFetch);
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: /add model/i }));
    await user.type(screen.getByLabelText('Name'), 'Auto');
    await user.type(screen.getByLabelText('ModelPack reference'), 'reg/x:1');
    await user.type(screen.getByLabelText('Memory'), '4');
    await user.click(screen.getByRole('button', { name: 'Create model' }));
    expect(screen.getByText(/"auto" is reserved/)).toBeTruthy();
    expect(backend.calls.some((c) => c.method === 'POST')).toBe(false);
  });

  it('edits from the row menu, opening the engine section for a model with engine args', async () => {
    const tuned: Model = { ...model, engine_args: ['--max-model-len', '8192'] };
    const backend = createBackend({
      'GET /v1/me': () => adminMe,
      'GET /v1/models': () => ({ models: [tuned] }),
      'GET /v1/deployments': () => ({ deployments: [] }),
      'PATCH /v1/models/m1': () => tuned,
    });
    render(<ModelsPage />, backend.addonFetch);
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Actions for qwen-7b' }));
    await user.click(await screen.findByRole('menuitem', { name: 'Edit' }));

    const args = (await screen.findByLabelText('Extra engine arguments')) as HTMLTextAreaElement;
    expect(args.value).toBe('--max-model-len\n8192');
    const memory = screen.getByLabelText('Memory') as HTMLInputElement;
    expect(memory.value).toBe('16');

    // Switching the unit converts the amount instead of reinterpreting it.
    await user.click(screen.getByRole('radio', { name: 'MB' }));
    expect(memory.value).toBe('16384');
    await user.clear(memory);
    await user.type(memory, '20000');
    await user.click(screen.getByRole('button', { name: 'Save changes' }));

    await waitFor(() => expect(backend.calls.some((c) => c.method === 'PATCH')).toBe(true));
    expect(backend.calls.find((c) => c.method === 'PATCH')?.body).toMatchObject({
      memory_mb: 20000,
      engine_args: ['--max-model-len', '8192'],
    });
  });

  it('shows a failed save inside the dialog and keeps it open', async () => {
    const backend = createBackend({
      'GET /v1/me': () => adminMe,
      'GET /v1/models': () => ({ models: [model] }),
      'GET /v1/deployments': () => ({ deployments: [] }),
      'POST /v1/models': () =>
        new Response(
          JSON.stringify({ error: { code: 'conflict', message: 'model "llama" already exists' } }),
          { status: 409, headers: { 'Content-Type': 'application/json' } },
        ),
    });
    render(<ModelsPage />, backend.addonFetch);
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: /add model/i }));
    await user.type(screen.getByLabelText('Name'), 'llama');
    await user.type(screen.getByLabelText('ModelPack reference'), 'reg/llama:1');
    await user.type(screen.getByLabelText('Memory'), '8');
    await user.click(screen.getByRole('button', { name: 'Create model' }));

    const dialog = within(screen.getByRole('dialog'));
    expect(await dialog.findByText('model "llama" already exists')).toBeTruthy();
    expect(dialog.getByRole('button', { name: 'Create model' })).toBeTruthy();
  });

  it('offers the add action in the empty state', async () => {
    const { addonFetch } = createBackend({
      'GET /v1/me': () => adminMe,
      'GET /v1/models': () => ({ models: [] }),
      'GET /v1/deployments': () => ({ deployments: [] }),
    });
    render(<ModelsPage />, addonFetch);
    expect(await screen.findByText('No models yet')).toBeTruthy();
    expect(await screen.findByRole('button', { name: /add model/i })).toBeTruthy();
  });

  it('hides the mutation controls for a non-admin', async () => {
    const { addonFetch, calls } = createBackend({
      'GET /v1/me': () => userMe,
      'GET /v1/models': () => ({ models: [model] }),
      'GET /v1/deployments': () => ({ deployments: [] }),
    });
    render(<ModelsPage />, addonFetch);
    await screen.findByText('qwen-7b');
    await waitFor(() => expect(calls.some((c) => c.path === '/v1/me')).toBe(true));
    expect(screen.queryByRole('button', { name: /add model/i })).toBeNull();
    expect(screen.queryByRole('button', { name: /actions for/i })).toBeNull();
  });
});

describe('PlacementPage', () => {
  const routes = (deployments: unknown[]) => ({
    'GET /v1/me': () => adminMe,
    'GET /v1/nodes': () => ({
      nodes: [node(), node({ name: 'worker-2', modelpack_csi: false })],
      warnings: ['cluster far-away did not answer'],
    }),
    'GET /v1/deployments': () => ({ deployments }),
    'GET /v1/models': () => ({ models: [model] }),
  });

  it('shows the nodes with their deployments and which nodes cannot take a model', async () => {
    const { addonFetch } = createBackend(routes([deployment]));
    render(<PlacementPage />, addonFetch);
    expect(await screen.findByText('cluster far-away did not answer')).toBeTruthy();

    const first = within(await screen.findByTestId('node-worker-1'));
    expect(await first.findByRole('button', { name: `Retry ${model.name}` })).toBeTruthy();
    const second = within(screen.getByTestId('node-worker-2'));
    expect(second.getByText(/No ModelPack CSI driver/)).toBeTruthy();
    expect(second.queryByRole('button', { name: /Add a model/ })).toBeNull();

    // The failure reason sits behind the failed badge, reachable by keyboard as well as hover.
    const user = userEvent.setup();
    await user.hover(first.getByRole('button', { name: 'failed' }));
    expect((await screen.findByRole('tooltip')).textContent).toContain('image pull failed');
  });

  it('places a model from the node menu without dragging', async () => {
    const { addonFetch } = createBackend(routes([]));
    render(<PlacementPage />, addonFetch);
    const user = userEvent.setup();
    const good = within(await screen.findByTestId('node-worker-1'));
    await user.click(await good.findByRole('button', { name: 'Add a model to worker-1' }));
    await user.click(await screen.findByRole('menuitem', { name: new RegExp(model.name) }));
    expect(good.getByText('new')).toBeTruthy();
    expect(good.getByText('16 GB free')).toBeTruthy();
    expect(screen.getByRole('status').textContent).toBe('1 to deploy');
  });

  it('stages a dropped model, updates the free capacity and deploys on apply', async () => {
    const { addonFetch, calls } = createBackend({
      ...routes([]),
      'POST /v1/deployments': () => created(deployment),
    });
    render(<PlacementPage />, addonFetch);
    const posts = () => calls.filter((c) => c.method === 'POST').map((c) => c.body);

    const card = await screen.findByTestId(`model-${model.name}`);
    await waitFor(() => expect(card.getAttribute('draggable')).toBe('true'));
    const good = await screen.findByTestId('node-worker-1');
    const bad = screen.getByTestId('node-worker-2');
    expect(within(good).getByText('32 GB free')).toBeTruthy();
    expect(within(good).getByText('1 of 1 free')).toBeTruthy();

    fireEvent.dragStart(card);
    expect(within(bad).getByText(/No ModelPack CSI driver/)).toBeTruthy();
    fireEvent.drop(bad);
    fireEvent.drop(good);

    expect(within(good).getByText('new')).toBeTruthy();
    expect(within(good).getByText('16 GB free')).toBeTruthy();
    expect(within(good).getByText('0 of 1 free')).toBeTruthy();
    expect(within(bad).queryByText('new')).toBeNull();
    expect(posts()).toEqual([]);

    const user = userEvent.setup();
    await user.click(screen.getByRole('button', { name: 'Apply' }));
    await waitFor(() =>
      expect(posts()).toEqual([{ model_id: model.id, cluster: 'edge', node: 'worker-1' }]),
    );
  });

  it('removes a deployment only once the plan is applied', async () => {
    const { addonFetch, calls } = createBackend({
      ...routes([deployment]),
      'DELETE /v1/deployments/d1': () => new Response(null, { status: 202 }),
    });
    render(<PlacementPage />, addonFetch);
    const deletes = () => calls.filter((c) => c.method === 'DELETE');

    const user = userEvent.setup();
    const first = within(await screen.findByTestId('node-worker-1'));
    await user.click(await first.findByRole('button', { name: `Take ${model.name} off this node` }));
    expect(first.getByText('to remove')).toBeTruthy();
    expect(first.getByText('1 of 1 free')).toBeTruthy();
    expect(deletes()).toEqual([]);

    await user.click(first.getByRole('button', { name: 'Keep' }));
    expect(first.queryByText('to remove')).toBeNull();

    await user.click(first.getByRole('button', { name: `Take ${model.name} off this node` }));
    await user.click(screen.getByRole('button', { name: 'Apply' }));
    await waitFor(() => expect(deletes()).toHaveLength(1));
  });
});

describe('placement plan', () => {
  const worker = node();
  const big: Model = { ...model, id: 'm2', name: 'big', memory_mb: 40000, vgpus: 0 };
  const ready: Deployment = { ...deployment, state: 'ready', error: '' };

  it('counts planned models against free memory and GPUs', () => {
    const plan = assign(EMPTY_PLAN, model, worker, []);
    const left = capacity(worker, slotsOn(worker, [], [model], plan));
    expect(left).toMatchObject({ memoryFree: 32768 - 16384, gpusFree: 0, gpusTotal: 1 });
  });

  it('gives back what a planned removal frees', () => {
    const slots = slotsOn(worker, [ready], [model], { added: [], removed: [ready.id] });
    expect(capacity(worker, slots)).toMatchObject({ memoryFree: 32768 + 16384, gpusFree: 1 });
    // Only the GPU of the model that stays is taken; memory already reflects it.
    expect(capacity(worker, slotsOn(worker, [ready], [model], EMPTY_PLAN))).toMatchObject({
      memoryFree: 32768,
      gpusFree: 0,
    });
  });

  it('refuses a model that does not fit or is already there', () => {
    expect(dropBlocker(worker, big, [])).toMatch(/Needs 39 GB/);
    const taken = slotsOn(worker, [ready], [model], EMPTY_PLAN);
    expect(dropBlocker(worker, model, taken)).toMatch(/already on this node/);
    expect(dropBlocker(worker, { ...big, memory_mb: 1, vgpus: 1 }, taken)).toMatch(/Needs 1 GPU, 0 free/);
  });

  it('keeps a deployment when its model is dropped back on the node', () => {
    const plan = assign({ added: [], removed: [ready.id] }, model, worker, [ready]);
    expect(plan).toEqual(EMPTY_PLAN);
  });

  it('drops entries whose subject is gone or already deployed', () => {
    const plan = { added: [{ modelId: model.id, cluster: 'edge', node: 'worker-1' }], removed: ['gone'] };
    expect(prune(plan, [ready], [model], [worker])).toEqual(EMPTY_PLAN);
  });
});

describe('KeysPage', () => {
  it('shows the secret once after creating a key', async () => {
    let keys = [key];
    const { addonFetch } = createBackend({
      'GET /v1/me': () => adminMe,
      'GET /v1/keys': () => ({ keys }),
      'POST /v1/keys': () => {
        keys = [...keys, { ...key, id: 'k2', name: 'ci' }];
        return created({ key: keys[1], secret: 'sk-secret-value' });
      },
    });
    render(<KeysPage />, addonFetch);
    expect(await screen.findByText('editor')).toBeTruthy();

    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: /create key/i }));
    await user.type(screen.getByLabelText('Name'), 'ci');
    await user.click(screen.getByRole('button', { name: 'Create key' }));

    expect((await screen.findByTestId('key-secret')).textContent).toBe('sk-secret-value');
    expect(screen.getByText(/will not be shown again/)).toBeTruthy();

    await user.click(screen.getByRole('button', { name: 'Done' }));
    await waitFor(() => expect(screen.queryByText('sk-secret-value')).toBeNull());
    expect(await screen.findByText('ci')).toBeTruthy();
  });
});

describe('KeysPage revoke', () => {
  it('revokes a key from the row menu after confirming', async () => {
    let revoked = false;
    const { addonFetch } = createBackend({
      'GET /v1/me': () => adminMe,
      'GET /v1/keys': () => ({ keys: revoked ? [] : [key] }),
      'DELETE /v1/keys/k1': () => {
        revoked = true;
        return new Response(null, { status: 204 });
      },
    });
    render(<KeysPage />, addonFetch);
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Actions for editor' }));
    await user.click(await screen.findByRole('menuitem', { name: /revoke/i }));
    await user.click(await screen.findByRole('button', { name: 'Revoke' }));
    expect(await screen.findByText('No API keys yet')).toBeTruthy();
  });
});

describe('SettingsPage', () => {
  it('shows the current strategy and keeps Save disabled until it changes', async () => {
    const { addonFetch } = createBackend({
      'GET /v1/settings': () => ({
        scheduler: { strategy: 'round-robin', config: null },
        strategies: ['round-robin', 'least-loaded'],
      }),
    });
    render(<SettingsPage />, addonFetch);
    const current = await screen.findByRole('radio', { name: 'round-robin' });
    expect(current.getAttribute('aria-checked')).toBe('true');
    const save = screen.getByRole('button', { name: 'Save' }) as HTMLButtonElement;
    expect(save.disabled).toBe(true);
    await userEvent.setup().click(screen.getByRole('radio', { name: 'least-loaded' }));
    expect(save.disabled).toBe(false);
  });
});
