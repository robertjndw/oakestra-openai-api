import {
  Button,
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
  Progress,
  StatusBadge,
  StatusDot,
} from '@oakestra/ui';
import {
  BanIcon,
  BoxesIcon,
  CirclePlusIcon,
  MinusIcon,
  PlusIcon,
  PowerOffIcon,
  RotateCwIcon,
  TriangleAlertIcon,
  Undo2Icon,
  XIcon,
} from 'lucide-react';
import { createContext, useContext, useState, type DragEvent } from 'react';

import { useRetryDeployment } from '../api';
import { DeploymentStateBadge, formatMb } from '../lib';
import { capacity, dropBlocker, nodeBlocker, type Slot } from '../placement-plan';
import type { Deployment, Model, Node } from '../types';
import { Hint } from './Canvas';

/** What is being dragged: a model from the palette, or one already sitting on a node. */
export interface Drag {
  model: Model;
  from?: { node: Node; slot: Slot };
}

/** Everything the cards on the canvas need, shared through context so the layout only places boxes. */
export interface Board {
  admin: boolean;
  models: readonly Model[];
  drag: Drag | null;
  slotsOn: (node: Node) => Slot[];
  onDrop: (node: Node) => void;
  /** The keyboard and menu path to the same result as dropping a palette model on `node`. */
  onPlace: (node: Node, model: Model) => void;
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

/** The model's color as a small square; a deployment whose model was deleted gets a grey one. */
export function Swatch({ modelId, className = 'oai:size-2.5' }: { modelId?: string; className?: string }) {
  return (
    <span
      aria-hidden
      className={`oai:shrink-0 oai:rounded-[3px] ${modelId ? '' : 'oai:bg-muted-foreground/40'} ${className}`}
      style={modelId ? { backgroundColor: modelColor(modelId) } : undefined}
    />
  );
}

/** "16 GB · 1 GPU", the figures that decide where a model fits. */
export const formatResources = (model: Model) =>
  `${formatMb(model.memory_mb)} · ${model.vgpus > 0 ? `${model.vgpus} GPU` : 'no GPU'}`;

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
// measuring it. These mirror the classes in WorkerCard and SlotChip: keep them in step when the
// markup changes.
//
// Card: 1px border top and bottom, p-3, rows separated by gap-3:
//   2 (border) + 24 (padding) + 36 (header, h-9) + 12 + 28 (meters or notice, h-7) + 12 = 114
// The slot area is one h-9 row per slot with space-y-1.5 between them, or a single h-9
// placeholder when the node is empty, so it is max(n, 1) * 36 + (max(n, 1) - 1) * 6.
// Folding the missing last gap into the base gives 114 - 6 + max(n, 1) * 42.
// Drag feedback is an absolutely positioned overlay, so a drag never changes a card's height.
export const CARD_WIDTH = 300;
const CARD_BASE_HEIGHT = 108;
const SLOT_HEIGHT = 42;
export const cardHeight = (slots: number) => CARD_BASE_HEIGHT + Math.max(slots, 1) * SLOT_HEIGHT;

export const GRAB = 'oai:cursor-grab oai:active:cursor-grabbing';

const slotKey = (slot: Slot) => (slot.kind === 'added' ? `new-${slot.model.id}` : slot.deployment.id);

function Meter({ label, value, used }: { label: string; value: string; used: number }) {
  return (
    <div className="oai:flex oai:min-w-0 oai:flex-col oai:gap-1.5">
      <div className="oai:flex oai:items-baseline oai:justify-between oai:gap-2 oai:text-xs oai:leading-4">
        <span className="oai:text-muted-foreground">{label}</span>
        <span className="oai:truncate oai:font-medium oai:tabular-nums">{value}</span>
      </div>
      <Progress value={Math.min(Math.max(used, 0), 1) * 100} aria-label={`${label} in use`} />
    </div>
  );
}

/** "new" or "to remove": how a staged slot differs from what runs today. */
function DiffTag({ kind }: { kind: 'added' | 'removing' }) {
  const added = kind === 'added';
  const Icon = added ? PlusIcon : MinusIcon;
  return (
    <span
      className={`oai:inline-flex oai:shrink-0 oai:items-center oai:gap-0.5 oai:rounded-sm oai:px-1.5 oai:text-xs oai:leading-5 oai:font-medium ${
        added ? 'oai:bg-success/12 oai:text-success' : 'oai:bg-destructive/10 oai:text-destructive'
      }`}
    >
      <Icon aria-hidden className="oai:size-3" />
      {added ? 'new' : 'to remove'}
    </span>
  );
}

function SlotChip({ node, slot }: { node: Node; slot: Slot }) {
  const board = useBoard();
  const retry = useRetryDeployment();
  const name = slot.kind === 'added' ? slot.model.name : slot.deployment.model_name;
  const removing = slot.kind === 'removing';
  const deployed = slot.kind === 'deployed' ? slot.deployment : null;
  // A deployment whose model was deleted cannot be moved, only removed.
  const movable = board.admin && !removing && slot.model !== undefined;
  const from = board.drag?.from;
  const lifted =
    from !== undefined &&
    from.node.cluster === node.cluster &&
    from.node.name === node.name &&
    slotKey(from.slot) === slotKey(slot);

  const surface =
    slot.kind === 'added'
      ? 'oai:border-dashed oai:border-success/50 oai:bg-success/5'
      : removing
        ? 'oai:border-dashed oai:border-destructive/40 oai:bg-destructive/5'
        : 'oai:bg-background';
  return (
    <li
      className={`oai:flex oai:h-9 oai:items-center oai:gap-2 oai:rounded-md oai:border oai:pr-1 oai:pl-2.5 oai:transition-opacity ${surface} ${
        lifted ? 'oai:opacity-40' : ''
      } ${movable ? GRAB : ''}`}
      {...dragProps(movable, name, () => board.onDragSlot(node, slot), board.onDragEnd)}
    >
      <Swatch modelId={slot.model?.id} className={`oai:size-2.5 ${removing ? 'oai:opacity-50' : ''}`} />
      <span
        className={`oai:min-w-0 oai:flex-1 oai:truncate oai:text-sm oai:font-medium ${
          removing ? 'oai:text-muted-foreground oai:line-through' : ''
        }`}
      >
        {name}
      </span>
      {slot.kind !== 'deployed' ? (
        <DiffTag kind={slot.kind} />
      ) : deployed?.state === 'failed' && deployed.error ? (
        // A title attribute never reaches keyboard and touch users, so the error gets a
        // focusable trigger.
        <Hint label={deployed.error}>
          <button type="button" className="oai:shrink-0 oai:cursor-help oai:rounded-sm oai:outline-none oai:focus-visible:ring-2 oai:focus-visible:ring-ring">
            <DeploymentStateBadge state="failed" />
          </button>
        </Hint>
      ) : deployed ? (
        <DeploymentStateBadge state={deployed.state} />
      ) : null}
      {board.admin ? (
        <span className="oai:flex oai:shrink-0 oai:items-center">
          {deployed?.state === 'failed' ? (
            <Hint label="Retry">
              <Button
                size="icon-xs"
                variant="ghost"
                aria-label={`Retry ${name}`}
                disabled={retry.isPending}
                onClick={() => retry.mutate(deployed)}
              >
                <RotateCwIcon aria-hidden />
              </Button>
            </Hint>
          ) : null}
          {slot.kind === 'removing' ? (
            <Hint label="Keep it on this node">
              <Button size="xs" variant="ghost" onClick={() => board.onRestore(slot.deployment)}>
                <Undo2Icon aria-hidden />
                Keep
              </Button>
            </Hint>
          ) : (
            <Hint label={slot.kind === 'added' ? 'Discard this placement' : 'Take off this node'}>
              <Button
                size="icon-xs"
                variant="ghost"
                aria-label={`Take ${name} off this node`}
                onClick={() => board.onUnassign(node, slot)}
              >
                <XIcon aria-hidden />
              </Button>
            </Hint>
          )}
        </span>
      ) : null}
    </li>
  );
}

/** Same result as dragging from the palette, for keyboard users. */
function AddModelMenu({ node, slots }: { node: Node; slots: readonly Slot[] }) {
  const board = useBoard();
  return (
    <DropdownMenu>
      <Hint label="Add a model">
        <DropdownMenuTrigger asChild>
          <Button size="icon-sm" variant="ghost" aria-label={`Add a model to ${node.name}`} className="oai:-mr-1.5">
            <PlusIcon aria-hidden />
          </Button>
        </DropdownMenuTrigger>
      </Hint>
      <DropdownMenuContent align="end" className="oai:w-64">
        <DropdownMenuLabel className="oai:truncate">Place on {node.name}</DropdownMenuLabel>
        <DropdownMenuSeparator />
        {board.models.length === 0 ? (
          <DropdownMenuItem disabled>No models yet</DropdownMenuItem>
        ) : (
          board.models.map((model) => {
            const refusal = dropBlocker(node, model, slots);
            return (
              <DropdownMenuItem
                key={model.id}
                disabled={refusal !== null}
                onSelect={() => board.onPlace(node, model)}
                className="oai:items-start"
              >
                <Swatch modelId={model.id} className="oai:mt-1.5 oai:size-2.5" />
                <span className="oai:min-w-0">
                  <span className="oai:block oai:truncate">{model.name}</span>
                  <span className="oai:block oai:text-xs oai:text-muted-foreground">
                    {refusal ?? formatResources(model)}
                  </span>
                </span>
              </DropdownMenuItem>
            );
          })
        )}
      </DropdownMenuContent>
    </DropdownMenu>
  );
}

/** Takes the meters' row on a node that cannot run models, so the reason is never cut short. */
function BlockedNotice({ node, reason }: { node: Node; reason: string }) {
  const Icon = node.active ? TriangleAlertIcon : PowerOffIcon;
  return (
    <p className="oai:flex oai:h-7 oai:items-center oai:gap-2 oai:rounded-md oai:bg-muted oai:px-2.5 oai:text-xs oai:leading-3.5 oai:text-muted-foreground">
      <Icon aria-hidden className={`oai:size-3.5 oai:shrink-0 ${node.active ? 'oai:text-warning' : ''}`} />
      <span className="oai:line-clamp-2">{reason}</span>
    </p>
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
  const dragging = drag !== null && !home;
  const refusal = dragging ? dropBlocker(node, drag.model, slots) : null;
  const accepts = dragging && refusal === null;
  const target = useDropTarget(accepts, () => board.onDrop(node));

  const frame = blocked
    ? `oai:bg-muted/40 ${dragging ? 'oai:opacity-60' : ''}`
    : accepts
      ? target.over
        ? 'oai:border-primary oai:bg-card oai:shadow-lg oai:ring-4 oai:ring-primary/15'
        : 'oai:border-primary/50 oai:bg-card oai:shadow-sm'
      : 'oai:bg-card oai:shadow-sm';

  return (
    <div
      data-testid={`node-${node.name}`}
      className={`oai:relative oai:flex oai:h-full oai:flex-col oai:gap-3 oai:rounded-xl oai:border oai:p-3 oai:transition-[box-shadow,border-color,opacity] ${frame}`}
      {...target.handlers}
    >
      <div className="oai:flex oai:h-9 oai:items-center oai:gap-2.5">
        <StatusDot tone={!node.active ? 'neutral' : blocked ? 'warning' : 'success'} />
        <div className="oai:min-w-0 oai:flex-1">
          <h3 className="oai:truncate oai:text-sm oai:leading-5 oai:font-semibold">{node.name}</h3>
          <p className="oai:truncate oai:text-xs oai:leading-4 oai:text-muted-foreground oai:tabular-nums">
            {node.vcpus} vCPU · {node.architecture} · {node.ip}
          </p>
        </div>
        {blocked ? (
          <StatusBadge tone={node.active ? 'warning' : 'neutral'} dot={false}>
            {node.active ? 'unavailable' : 'inactive'}
          </StatusBadge>
        ) : board.admin ? (
          <AddModelMenu node={node} slots={slots} />
        ) : null}
      </div>
      {blocked ? (
        <BlockedNotice node={node} reason={blocked} />
      ) : (
        <div className="oai:grid oai:h-7 oai:grid-cols-2 oai:gap-x-4">
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
      )}
      {slots.length > 0 ? (
        <ul className="oai:space-y-1.5">
          {slots.map((slot) => (
            <SlotChip key={slotKey(slot)} node={node} slot={slot} />
          ))}
        </ul>
      ) : (
        <p className="oai:flex oai:h-9 oai:items-center oai:justify-center oai:rounded-md oai:bg-muted/50 oai:text-xs oai:text-muted-foreground">
          No models on this node
        </p>
      )}
      {dragging && !blocked ? (
        // Sits over everything below the header (top-[54px] is half way through the gap under
        // the h-9 header), so the node stays identifiable and the card keeps its height. A
        // blocked node already says why in its notice and gets no second copy here.
        <div
          className={`oai:pointer-events-none oai:absolute oai:inset-x-1.5 oai:top-[54px] oai:bottom-1.5 oai:flex oai:items-center oai:justify-center oai:gap-2 oai:rounded-lg oai:px-4 oai:text-center oai:backdrop-blur-[1px] oai:transition-colors ${
            accepts
              ? target.over
                ? 'oai:border-2 oai:border-primary oai:bg-card/90 oai:text-primary'
                : 'oai:border-2 oai:border-dashed oai:border-primary/50 oai:bg-card/85 oai:text-primary'
              : 'oai:border oai:border-dashed oai:bg-muted/90 oai:text-muted-foreground'
          }`}
        >
          {accepts ? (
            <>
              <CirclePlusIcon aria-hidden className="oai:size-4 oai:shrink-0" />
              <span className="oai:min-w-0 oai:truncate oai:text-sm oai:font-medium">
                Drop to place {drag.model.name}
              </span>
            </>
          ) : (
            <>
              <BanIcon aria-hidden className="oai:size-4 oai:shrink-0" />
              <span className="oai:line-clamp-3 oai:text-xs oai:leading-4">{refusal}</span>
            </>
          )}
        </div>
      ) : null}
    </div>
  );
}

export function ClusterFrame({ name, workers }: { name: string; workers: number }) {
  return (
    <section
      aria-label={`Cluster ${name}`}
      // Empty space inside a frame pans the canvas like the background does.
      data-canvas-surface
      className="oai:h-full oai:rounded-2xl oai:border oai:bg-background/70 oai:px-5 oai:pt-4 oai:shadow-xs"
    >
      {/* Click-through, so grabbing the title pans like the rest of the frame. */}
      <header className="oai:pointer-events-none oai:flex oai:h-6 oai:items-center oai:gap-2">
        <BoxesIcon aria-hidden className="oai:size-4 oai:text-muted-foreground" />
        <h2 className="oai:text-sm oai:font-semibold">{name}</h2>
        <span className="oai:rounded-full oai:bg-muted oai:px-2 oai:text-xs oai:leading-5 oai:text-muted-foreground oai:tabular-nums">
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
// 1px border + pt-4 + the h-6 title, then 16px before the first row of cards.
const FRAME_HEADER = 57;
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
