import { Button, Progress, StatusBadge } from '@oakestra/ui';
import { RotateCwIcon, XIcon } from 'lucide-react';
import { createContext, useContext, useState, type CSSProperties, type DragEvent } from 'react';

import { useRetryDeployment } from '../api';
import { DeploymentStateBadge, formatMb } from '../lib';
import { capacity, dropBlocker, nodeBlocker, type Slot } from '../placement-plan';
import type { Deployment, Model, Node } from '../types';

/** What is being dragged: a model from the palette, or one already sitting on a node. */
export interface Drag {
  model: Model;
  from?: { node: Node; slot: Slot };
}

/** Everything the cards on the canvas need; React Flow only hands them their own data. */
export interface Board {
  admin: boolean;
  drag: Drag | null;
  slotsOn: (node: Node) => Slot[];
  onDrop: (node: Node) => void;
  onDragSlot: (node: Node, slot: Slot) => void;
  onDragEnd: () => void;
  onUnassign: (node: Node, slot: Slot) => void;
  onRestore: (deployment: Deployment) => void;
}

export const BoardContext = createContext<Board | null>(null);

function useBoard(): Board {
  const board = useContext(BoardContext);
  if (!board) throw new Error('placement cards must render inside BoardContext');
  return board;
}

// Sticky-note hues. A model keeps its color everywhere it shows up, which makes it easy to spot
// where each one runs when the canvas is zoomed out.
const HUES = [212, 152, 32, 282, 352, 182, 52, 322];

export function modelColor(modelId: string): string {
  let hash = 0;
  for (const char of modelId) hash = (hash * 31 + char.charCodeAt(0)) >>> 0;
  return `hsl(${HUES[hash % HUES.length]} 70% 50%)`;
}

const accent = (modelId: string | undefined): CSSProperties =>
  modelId ? { borderLeftColor: modelColor(modelId), borderLeftWidth: 4 } : {};

/** Handlers that make an element a drop target while `accepts` holds. */
export function useDropTarget(accepts: boolean, onDrop: () => void) {
  const [over, setOver] = useState(false);
  return {
    over: over && accepts,
    handlers: {
      onDragOver: (event: DragEvent) => {
        if (!accepts) return;
        // Without this the browser refuses the drop.
        event.preventDefault();
        setOver(true);
      },
      onDragLeave: (event: DragEvent) => {
        // Moving onto a child also fires dragleave; only a real exit clears the highlight.
        if (!event.currentTarget.contains(event.relatedTarget as globalThis.Node | null)) {
          setOver(false);
        }
      },
      onDrop: (event: DragEvent) => {
        setOver(false);
        if (!accepts) return;
        event.preventDefault();
        onDrop();
      },
    },
  };
}

export function dragProps(enabled: boolean, label: string, onStart: () => void, onEnd: () => void) {
  if (!enabled) return {};
  return {
    draggable: true,
    onDragStart: (event: DragEvent) => {
      // Firefox only starts a drag once some data is set.
      event.dataTransfer?.setData('text/plain', label);
      onStart();
    },
    onDragEnd: onEnd,
  };
}

// The canvas gives every card a fixed box, so the layout has to know a card's height without
// measuring it. These mirror the classes below: keep them in step when the markup changes.
export const CARD_WIDTH = 300;
const CARD_BASE_HEIGHT = 154;
const SLOT_HEIGHT = 44;
export const cardHeight = (slots: number) => CARD_BASE_HEIGHT + slots * SLOT_HEIGHT;

export const GRAB = 'oai:cursor-grab oai:active:cursor-grabbing';

function Meter({ label, value, used }: { label: string; value: string; used: number }) {
  return (
    <div className="oai:space-y-1">
      <div className="oai:flex oai:justify-between oai:text-xs oai:leading-4">
        <span className="oai:text-muted-foreground">{label}</span>
        <span className="oai:font-medium">{value}</span>
      </div>
      <Progress value={Math.min(Math.max(used, 0), 1) * 100} aria-label={`${label} in use`} />
    </div>
  );
}

function SlotChip({ node, slot }: { node: Node; slot: Slot }) {
  const board = useBoard();
  const retry = useRetryDeployment();
  const name = slot.kind === 'added' ? slot.model.name : slot.deployment.model_name;
  const removing = slot.kind === 'removing';
  const error = slot.kind === 'deployed' ? slot.deployment.error : '';
  // A deployment whose model was deleted cannot be moved, only removed.
  const movable = board.admin && !removing && slot.model !== undefined;
  return (
    <li
      className={`oai:flex oai:h-9 oai:items-center oai:gap-2 oai:rounded-md oai:border oai:bg-background oai:pr-1 oai:pl-2 ${
        slot.kind === 'added' ? 'oai:border-dashed' : ''
      } ${removing ? 'oai:opacity-60' : movable ? GRAB : ''}`}
      style={accent(slot.model?.id)}
      title={error || undefined}
      {...dragProps(movable, name, () => board.onDragSlot(node, slot), board.onDragEnd)}
    >
      <span
        className={`oai:min-w-0 oai:truncate oai:text-sm oai:font-medium ${removing ? 'oai:line-through' : ''}`}
      >
        {name}
      </span>
      {slot.kind === 'added' ? (
        <StatusBadge tone="info">new</StatusBadge>
      ) : removing ? (
        <StatusBadge tone="neutral">to remove</StatusBadge>
      ) : (
        <DeploymentStateBadge state={slot.deployment.state} />
      )}
      {board.admin ? (
        <span className="oai:ml-auto oai:flex oai:shrink-0 oai:items-center">
          {slot.kind === 'deployed' && slot.deployment.state === 'failed' ? (
            <Button
              size="icon-sm"
              variant="ghost"
              aria-label={`Retry ${name}`}
              disabled={retry.isPending}
              onClick={() => retry.mutate(slot.deployment)}
            >
              <RotateCwIcon aria-hidden />
            </Button>
          ) : null}
          {removing ? (
            <Button size="sm" variant="ghost" onClick={() => board.onRestore(slot.deployment)}>
              Keep
            </Button>
          ) : (
            <Button
              size="icon-sm"
              variant="ghost"
              aria-label={`Take ${name} off this node`}
              onClick={() => board.onUnassign(node, slot)}
            >
              <XIcon aria-hidden />
            </Button>
          )}
        </span>
      ) : null}
    </li>
  );
}

export function WorkerCard({ node }: { node: Node }) {
  const board = useBoard();
  const { drag } = board;
  const slots = board.slotsOn(node);
  const left = capacity(node, slots);
  const blocked = nodeBlocker(node);
  // Dragging a model within its own node changes nothing, so the node is not a target then.
  const home = drag?.from?.node === node;
  const refusal = drag && !home ? dropBlocker(node, drag.model, slots) : null;
  const accepts = drag !== null && !home && refusal === null;
  const target = useDropTarget(accepts, () => board.onDrop(node));

  const frame =
    !drag || home
      ? blocked
        ? 'oai:border-dashed oai:bg-muted'
        : 'oai:bg-card oai:shadow-sm'
      : accepts
        ? target.over
          ? 'oai:border-primary oai:bg-card oai:shadow-lg oai:ring-2 oai:ring-primary'
          : 'oai:border-primary oai:bg-card oai:shadow-sm'
        : 'oai:border-dashed oai:bg-muted oai:opacity-50';
  const footer =
    drag && !home ? (refusal ?? `Drop to put ${drag.model.name} here`) : (blocked ?? '');

  return (
    <div
      data-testid={`node-${node.name}`}
      className={`oai:flex oai:h-full oai:flex-col oai:gap-3 oai:rounded-xl oai:border oai:p-4 oai:transition-all ${frame}`}
      {...target.handlers}
    >
      <div className="oai:flex oai:h-6 oai:items-center oai:gap-2">
        <span
          aria-hidden
          className={`oai:size-2 oai:shrink-0 oai:rounded-full ${node.active ? 'oai:bg-success' : 'oai:bg-muted-foreground'}`}
        />
        <h3 className="oai:min-w-0 oai:truncate oai:font-medium">{node.name}</h3>
        <span className="oai:ml-auto oai:shrink-0 oai:text-xs oai:text-muted-foreground">
          {node.vcpus} vCPU, {node.architecture}
        </span>
      </div>
      <div className="oai:grid oai:grid-cols-2 oai:gap-4">
        <Meter
          label="Memory"
          value={`${formatMb(left.memoryFree)} free`}
          used={left.memoryMax > 0 ? 1 - left.memoryFree / left.memoryMax : 0}
        />
        <Meter
          label="GPU"
          value={`${left.gpusFree} of ${left.gpusTotal} free`}
          used={left.gpusTotal > 0 ? 1 - left.gpusFree / left.gpusTotal : 0}
        />
      </div>
      {slots.length > 0 ? (
        <ul className="oai:space-y-2">
          {slots.map((slot) => (
            <SlotChip
              key={slot.kind === 'added' ? `new-${slot.model.id}` : slot.deployment.id}
              node={node}
              slot={slot}
            />
          ))}
        </ul>
      ) : null}
      {footer ? (
        <p
          title={footer}
          className={`oai:mt-auto oai:truncate oai:text-xs oai:leading-8 ${accepts ? 'oai:text-muted-foreground' : 'oai:text-warning'}`}
        >
          {footer}
        </p>
      ) : (
        <p className="oai:mt-auto oai:rounded-md oai:border oai:border-dashed oai:text-center oai:text-xs oai:leading-[30px] oai:text-muted-foreground">
          {board.admin ? 'Drop a model here' : slots.length === 0 ? 'No models' : ' '}
        </p>
      )}
    </div>
  );
}

export function ClusterFrame({ name, workers }: { name: string; workers: number }) {
  return (
    <section
      aria-label={`Cluster ${name}`}
      // Empty space inside a frame pans the canvas like the background does.
      data-canvas-surface
      className="oai:h-full oai:rounded-2xl oai:border oai:border-dashed oai:bg-background/60 oai:px-5 oai:py-3"
    >
      <header className="oai:flex oai:items-baseline oai:gap-2">
        <h2 className="oai:font-medium">{name}</h2>
        <span className="oai:text-xs oai:text-muted-foreground">
          {workers} {workers === 1 ? 'worker' : 'workers'}
        </span>
      </header>
    </section>
  );
}

interface Box {
  x: number;
  y: number;
  width: number;
  height: number;
}

export type BoardItem =
  | (Box & { kind: 'cluster'; name: string; workers: number })
  | (Box & { kind: 'worker'; node: Node });

const FRAME_PADDING = 20;
const FRAME_HEADER = 44;
const GAP = 20;
const FRAME_GAP = 56;

/**
 * Lays clusters out side by side as frames with their workers in a grid, in canvas coordinates.
 * Depends on the data only, so a card grows in place when a model lands on it.
 */
export function layoutBoard(
  nodes: readonly Node[],
  slotCount: (node: Node) => number,
): { items: BoardItem[]; width: number; height: number } {
  const clusters = new Map<string, Node[]>();
  for (const node of nodes) clusters.set(node.cluster, [...(clusters.get(node.cluster) ?? []), node]);

  const items: BoardItem[] = [];
  let x = 0;
  let tallest = 0;
  for (const [cluster, workers] of [...clusters].sort(([a], [b]) => a.localeCompare(b))) {
    const columns = workers.length <= 1 ? 1 : workers.length <= 4 ? 2 : 3;
    const cards: BoardItem[] = [];
    let y = FRAME_HEADER;
    for (let start = 0; start < workers.length; start += columns) {
      const row = workers.slice(start, start + columns);
      row.forEach((node, column) => {
        cards.push({
          kind: 'worker',
          node,
          x: x + FRAME_PADDING + column * (CARD_WIDTH + GAP),
          y,
          width: CARD_WIDTH,
          height: cardHeight(slotCount(node)),
        });
      });
      y += Math.max(...row.map((node) => cardHeight(slotCount(node)))) + GAP;
    }
    const width = 2 * FRAME_PADDING + columns * CARD_WIDTH + (columns - 1) * GAP;
    const height = y - GAP + FRAME_PADDING;
    items.push({ kind: 'cluster', name: cluster, workers: workers.length, x, y: 0, width, height });
    items.push(...cards);
    tallest = Math.max(tallest, height);
    x += width + FRAME_GAP;
  }
  return { items, width: Math.max(x - FRAME_GAP, 0), height: tallest };
}
