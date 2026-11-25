/** Wire types of the backend's admin API (`/v1/...` below the addon API root). Times are unix ms. */

export interface Me {
  name: string;
  roles: string[];
  admin: boolean;
}

export interface Overview {
  public_url: string;
  version: string;
  oakestra: { reachable: boolean; error?: string };
  models: number;
  deployments: { total: number; ready: number };
  keys: number;
  scheduler: string;
}

export interface Model {
  id: string;
  name: string;
  reference: string;
  engine_image: string;
  engine_args: string[];
  command: string[] | null;
  container_port: number;
  memory_mb: number;
  vgpus: number;
  auto: boolean;
  labels: Record<string, string>;
  created_at: number;
  updated_at: number;
}

export interface ModelInput {
  name: string;
  reference: string;
  /** Empty selects the backend's default vLLM image. */
  engine_image?: string;
  engine_args?: string[];
  command?: string[] | null;
  container_port?: number;
  memory_mb?: number;
  vgpus?: number;
  auto?: boolean;
  labels?: Record<string, string>;
}

export interface Node {
  cluster: string;
  name: string;
  ip: string;
  architecture: string;
  active: boolean;
  vcpus: number;
  /** Available, not total. */
  memory_mb: number;
  vgpus: number;
  vram_mb: number;
  csi_drivers: string[];
  modelpack_csi: boolean;
}

export interface NodesResponse {
  nodes: Node[];
  warnings: string[];
}

export type DeploymentState =
  | 'pending'
  | 'scheduling'
  | 'starting'
  | 'ready'
  | 'unhealthy'
  | 'failed'
  | 'deleting';

export interface Deployment {
  id: string;
  model_id: string;
  model_name: string;
  cluster: string;
  node: string;
  state: DeploymentState;
  error: string;
  created_at: number;
  updated_at: number;
}

export interface Key {
  id: string;
  name: string;
  prefix: string;
  created_at: number;
  last_used_at: number | null;
}

export interface CreatedKey {
  key: Key;
  /** Only returned once, by the create call. */
  secret: string;
}

export interface Settings {
  scheduler: { strategy: string; config: unknown };
  strategies: string[];
}

export interface ErrorBody {
  error?: { code?: string; message?: string };
}
