import { addonQueryKey, useAddonApi, useNotify, type AddonApi } from '@oakestra/dashboard-sdk';
import { queryOptions, useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useMemo } from 'react';
import { ADDON_ID } from 'virtual:oakestra-addon';

import type {
  CreatedKey,
  Deployment,
  ErrorBody,
  Key,
  Me,
  Model,
  ModelInput,
  NodesResponse,
  Overview,
  Settings,
} from './types';

/** A failed backend call. `message` is the backend's own text and safe to show as it is. */
export class BackendError extends Error {
  readonly status: number;
  readonly code: string;

  constructor(status: number, code: string, message: string) {
    super(message);
    this.name = 'BackendError';
    this.status = status;
    this.code = code;
  }
}

async function request<T>(api: AddonApi, path: string, init: RequestInit = {}): Promise<T> {
  const headers: Record<string, string> = { Accept: 'application/json' };
  if (init.body !== undefined) headers['Content-Type'] = 'application/json';
  let response: Response;
  try {
    response = await api.fetch(path, { ...init, headers });
  } catch (error) {
    if (init.signal?.aborted) throw error;
    throw new BackendError(0, 'network', 'The OpenAI API backend is not reachable.');
  }
  if (!response.ok) {
    let body: ErrorBody = {};
    try {
      body = (await response.json()) as ErrorBody;
    } catch {
      // nginx answers 502 and 504 with HTML while the backend is down; the status says enough.
    }
    throw new BackendError(
      response.status,
      body.error?.code ?? 'unknown',
      body.error?.message || `The backend answered with HTTP ${response.status}.`,
    );
  }
  const text = await response.text();
  return (text ? JSON.parse(text) : undefined) as T;
}

const jsonBody = (method: string, body: unknown): RequestInit => ({
  method,
  body: JSON.stringify(body),
});

/** The typed backend calls, bound to one addon API client. */
export function createClient(api: AddonApi) {
  return {
    me: (signal?: AbortSignal) => request<Me>(api, '/v1/me', { signal }),
    overview: (signal?: AbortSignal) => request<Overview>(api, '/v1/overview', { signal }),
    models: (signal?: AbortSignal) =>
      request<{ models: Model[] }>(api, '/v1/models', { signal }).then((r) => r.models),
    createModel: (input: ModelInput) =>
      request<Model>(api, '/v1/models', jsonBody('POST', input)),
    updateModel: (id: string, input: Partial<ModelInput>) =>
      request<Model>(api, `/v1/models/${encodeURIComponent(id)}`, jsonBody('PATCH', input)),
    deleteModel: (id: string) =>
      request<void>(api, `/v1/models/${encodeURIComponent(id)}`, { method: 'DELETE' }),
    nodes: (signal?: AbortSignal) => request<NodesResponse>(api, '/v1/nodes', { signal }),
    deployments: (signal?: AbortSignal) =>
      request<{ deployments: Deployment[] }>(api, '/v1/deployments', { signal }).then(
        (r) => r.deployments,
      ),
    createDeployment: (input: { model_id: string; cluster: string; node: string }) =>
      request<Deployment>(api, '/v1/deployments', jsonBody('POST', input)),
    deleteDeployment: (id: string) =>
      request<void>(api, `/v1/deployments/${encodeURIComponent(id)}`, { method: 'DELETE' }),
    retryDeployment: (id: string) =>
      request<void>(api, `/v1/deployments/${encodeURIComponent(id)}/retry`, { method: 'POST' }),
    keys: (signal?: AbortSignal) =>
      request<{ keys: Key[] }>(api, '/v1/keys', { signal }).then((r) => r.keys),
    createKey: (name: string) => request<CreatedKey>(api, '/v1/keys', jsonBody('POST', { name })),
    deleteKey: (id: string) =>
      request<void>(api, `/v1/keys/${encodeURIComponent(id)}`, { method: 'DELETE' }),
    settings: (signal?: AbortSignal) => request<Settings>(api, '/v1/settings', { signal }),
    saveSettings: (scheduler: Settings['scheduler']) =>
      request<Settings>(api, '/v1/settings', jsonBody('PUT', { scheduler })),
  };
}

export type Client = ReturnType<typeof createClient>;

export function useClient(): Client {
  const api = useAddonApi();
  return useMemo(() => createClient(api), [api]);
}

// The shell and all addons share one QueryClient, so the addon id keeps the keys apart.
const key = (...parts: string[]) => addonQueryKey(ADDON_ID, ...parts);

const POLL_FAST_MS = 3_000;
const POLL_SLOW_MS = 15_000;
const TRANSIENT = new Set(['pending', 'scheduling', 'starting', 'deleting']);

export const useMe = () => {
  const client = useClient();
  return useQuery({
    queryKey: key('me'),
    queryFn: ({ signal }) => client.me(signal),
    staleTime: 60_000,
  });
};

/**
 * Whether the backend grants the caller admin rights. The backend enforces mutations, so until
 * /me has answered (or when it fails) the controls stay hidden rather than showing and failing
 * with a 403.
 */
export const useIsAdmin = (): boolean => useMe().data?.admin ?? false;

export const useOverview = () => {
  const client = useClient();
  return useQuery({
    queryKey: key('overview'),
    queryFn: ({ signal }) => client.overview(signal),
    refetchInterval: POLL_SLOW_MS,
  });
};

export const useModels = () => {
  const client = useClient();
  return useQuery({ queryKey: key('models'), queryFn: ({ signal }) => client.models(signal) });
};

export const useNodes = () => {
  const client = useClient();
  return useQuery({
    queryKey: key('nodes'),
    queryFn: ({ signal }) => client.nodes(signal),
    refetchInterval: POLL_SLOW_MS,
  });
};

export const useDeployments = () => {
  const client = useClient();
  return useQuery({
    queryKey: key('deployments'),
    queryFn: ({ signal }) => client.deployments(signal),
    // Poll quickly only while something is changing, so an idle page stays quiet.
    refetchInterval: (query) =>
      query.state.data?.some((d) => TRANSIENT.has(d.state)) ? POLL_FAST_MS : POLL_SLOW_MS,
  });
};

export const useKeys = () => {
  const client = useClient();
  return useQuery({ queryKey: key('keys'), queryFn: ({ signal }) => client.keys(signal) });
};

export const useSettings = () => {
  const client = useClient();
  return useQuery(
    queryOptions({ queryKey: key('settings'), queryFn: ({ signal }) => client.settings(signal) }),
  );
};

// A failed mutation is toasted by the shell's MutationCache with meta.errorMessage as the title.
function useApiMutation<V, R>(
  run: (client: Client, variables: V) => Promise<R>,
  options: {
    errorMessage: string;
    invalidate: string[];
    success?: (result: R, variables: V) => string;
  },
) {
  const client = useClient();
  const queryClient = useQueryClient();
  const notify = useNotify();
  return useMutation({
    mutationFn: (variables: V) => run(client, variables),
    meta: { errorMessage: options.errorMessage },
    onSuccess: async (result, variables) => {
      await Promise.all(
        options.invalidate.map((part) => queryClient.invalidateQueries({ queryKey: key(part) })),
      );
      if (options.success) notify.success(options.success(result, variables));
    },
  });
}

export const useSaveModel = () =>
  useApiMutation(
    (c, v: { id?: string; input: ModelInput }) =>
      v.id ? c.updateModel(v.id, v.input) : c.createModel(v.input),
    {
      errorMessage: 'Could not save the model',
      invalidate: ['models', 'overview'],
      success: (model) => `Model ${model.name} saved`,
    },
  );

export const useDeleteModel = () =>
  useApiMutation((c, model: Model) => c.deleteModel(model.id), {
    errorMessage: 'Could not delete the model',
    invalidate: ['models', 'overview'],
    success: (_, model) => `Model ${model.name} deleted`,
  });

export const useCreateDeployment = () =>
  useApiMutation(
    (c, input: { model_id: string; cluster: string; node: string }) => c.createDeployment(input),
    {
      errorMessage: 'Could not deploy the model',
      invalidate: ['deployments', 'overview'],
      success: (d) => `Deploying ${d.model_name} to ${d.node}`,
    },
  );

export const useDeleteDeployment = () =>
  useApiMutation((c, d: Deployment) => c.deleteDeployment(d.id), {
    errorMessage: 'Could not remove the deployment',
    invalidate: ['deployments', 'overview'],
    success: (_, d) => `Removing ${d.model_name} from ${d.node}`,
  });

export const useRetryDeployment = () =>
  useApiMutation((c, d: Deployment) => c.retryDeployment(d.id), {
    errorMessage: 'Could not retry the deployment',
    invalidate: ['deployments', 'overview'],
  });

// The create dialog shows failures inline (it keeps the secret on screen), so it opts out of the
// shell's error toast.
export function useCreateKey() {
  const client = useClient();
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (name: string) => client.createKey(name),
    meta: { silent: true },
    onSuccess: () =>
      Promise.all(
        ['keys', 'overview'].map((part) => queryClient.invalidateQueries({ queryKey: key(part) })),
      ),
  });
}

export const useDeleteKey = () =>
  useApiMutation((c, k: Key) => c.deleteKey(k.id), {
    errorMessage: 'Could not revoke the key',
    invalidate: ['keys', 'overview'],
    success: (_, k) => `Key ${k.name} revoked`,
  });

export const useSaveSettings = () =>
  useApiMutation((c, scheduler: Settings['scheduler']) => c.saveSettings(scheduler), {
    errorMessage: 'Could not save the settings',
    invalidate: ['settings', 'overview'],
    success: () => 'Settings saved',
  });
