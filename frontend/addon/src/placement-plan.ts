import { formatMb } from './lib';
import type { Deployment, Model, Node } from './types';

/** A model the user put on a node but has not applied yet. */
export interface Assignment {
  modelId: string;
  cluster: string;
  node: string;
}

/** Staged changes: nothing reaches the backend until the user applies them. */
export interface Plan {
  added: Assignment[];
  /** Ids of existing deployments to remove. */
  removed: string[];
}

export const EMPTY_PLAN: Plan = { added: [], removed: [] };

export const planSize = (plan: Plan) => plan.added.length + plan.removed.length;

/** What sits on a node in the planned picture. */
export type Slot =
  | { kind: 'deployed'; model: Model | undefined; deployment: Deployment }
  | { kind: 'removing'; model: Model | undefined; deployment: Deployment }
  | { kind: 'added'; model: Model };

const sameNode = (a: { cluster: string; node: string }, node: Node) =>
  a.cluster === node.cluster && a.node === node.name;

export function slotsOn(
  node: Node,
  deployments: readonly Deployment[],
  models: readonly Model[],
  plan: Plan,
): Slot[] {
  const byId = new Map(models.map((model) => [model.id, model]));
  const slots: Slot[] = deployments
    .filter((d) => sameNode(d, node))
    .map((deployment) => ({
      kind: plan.removed.includes(deployment.id) ? 'removing' : 'deployed',
      model: byId.get(deployment.model_id),
      deployment,
    }));
  for (const a of plan.added) {
    const model = byId.get(a.modelId);
    if (model && sameNode(a, node)) slots.push({ kind: 'added', model });
  }
  return slots;
}

export interface Capacity {
  /** Free memory once the plan is applied. */
  memoryFree: number;
  /** The most memory the plan could free up: what is free now plus what planned removals release. */
  memoryMax: number;
  gpusFree: number;
  gpusTotal: number;
}

/**
 * What a node has left under the plan. Oakestra reports free memory, which already accounts for
 * the engines running there, so only planned changes move it. GPUs are reported as a total, so
 * the models staying on the node are subtracted as well.
 */
export function capacity(node: Node, slots: readonly Slot[]): Capacity {
  let memoryFree = node.memory_mb;
  let memoryMax = node.memory_mb;
  let gpusFree = node.vgpus;
  for (const slot of slots) {
    const memory = slot.model?.memory_mb ?? 0;
    const gpus = slot.model?.vgpus ?? 0;
    if (slot.kind === 'removing') {
      memoryFree += memory;
      memoryMax += memory;
    } else {
      gpusFree -= gpus;
      if (slot.kind === 'added') memoryFree -= memory;
    }
  }
  return { memoryFree, memoryMax, gpusFree: Math.max(gpusFree, 0), gpusTotal: node.vgpus };
}

/** Why a node cannot take any model, or null when it can. */
export function nodeBlocker(node: Node): string | null {
  if (!node.active) return 'This node is inactive.';
  if (!node.modelpack_csi) return 'No ModelPack CSI driver on this node.';
  return null;
}

/** Why this model cannot go on this node under the current plan, or null when it can. */
export function dropBlocker(node: Node, model: Model, slots: readonly Slot[]): string | null {
  const blocked = nodeBlocker(node);
  if (blocked) return blocked;
  if (slots.some((slot) => slot.kind !== 'removing' && slot.model?.id === model.id)) {
    return `${model.name} is already on this node.`;
  }
  const left = capacity(node, slots);
  if (model.vgpus > left.gpusFree) {
    return `Needs ${model.vgpus} GPU, ${left.gpusFree} free.`;
  }
  if (model.memory_mb > left.memoryFree) {
    return `Needs ${formatMb(model.memory_mb)}, ${formatMb(left.memoryFree)} free.`;
  }
  return null;
}

/** Puts a model on a node. Dropping back a model that was planned for removal just keeps it. */
export function assign(plan: Plan, model: Model, node: Node, deployments: readonly Deployment[]): Plan {
  const existing = deployments.find((d) => d.model_id === model.id && sameNode(d, node));
  if (existing && plan.removed.includes(existing.id)) return restore(plan, existing.id);
  return {
    ...plan,
    added: [...plan.added, { modelId: model.id, cluster: node.cluster, node: node.name }],
  };
}

/** Takes a slot off its node: a planned one is dropped, a deployed one is marked for removal. */
export function unassign(plan: Plan, slot: Slot, node: Node): Plan {
  if (slot.kind === 'added') {
    return {
      ...plan,
      added: plan.added.filter((a) => !(a.modelId === slot.model.id && sameNode(a, node))),
    };
  }
  if (plan.removed.includes(slot.deployment.id)) return plan;
  return { ...plan, removed: [...plan.removed, slot.deployment.id] };
}

export const restore = (plan: Plan, deploymentId: string): Plan => ({
  ...plan,
  removed: plan.removed.filter((id) => id !== deploymentId),
});

/**
 * Drops plan entries that no longer refer to anything: a deployment that is gone, a model or node
 * that disappeared, or an assignment that became a real deployment in the meantime.
 */
export function prune(
  plan: Plan,
  deployments: readonly Deployment[],
  models: readonly Model[],
  nodes: readonly Node[],
): Plan {
  const removed = plan.removed.filter((id) => deployments.some((d) => d.id === id));
  const added = plan.added.filter(
    (a) =>
      models.some((model) => model.id === a.modelId) &&
      nodes.some((node) => sameNode(a, node)) &&
      !deployments.some((d) => d.model_id === a.modelId && d.cluster === a.cluster && d.node === a.node),
  );
  return removed.length === plan.removed.length && added.length === plan.added.length
    ? plan
    : { added, removed };
}
