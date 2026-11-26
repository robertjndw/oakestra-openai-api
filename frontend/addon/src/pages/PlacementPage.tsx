import {
  Alert,
  AlertDescription,
  AlertTitle,
  Badge,
  Button,
  EmptyState,
  PageHeader,
  QueryErrorAlert,
  Skeleton,
  TooltipProvider,
} from '@oakestra/ui';
import {
  ArrowDownToLineIcon,
  CheckIcon,
  GripVerticalIcon,
  LoaderCircleIcon,
  MinusIcon,
  PlusIcon,
  ServerIcon,
  TriangleAlertIcon,
} from 'lucide-react';
import { useEffect, useMemo, useState } from 'react';

import {
  useCreateDeployment,
  useDeleteDeployment,
  useDeployments,
  useIsAdmin,
  useModels,
  useNodes,
} from '../api';
import {
  assign,
  EMPTY_PLAN,
  planSize,
  prune,
  restore,
  slotsOn,
  unassign,
  type Plan,
} from '../placement-plan';
import type { Model, Node } from '../types';
import { Canvas } from './Canvas';
import {
  BoardContext,
  ClusterFrame,
  dragProps,
  formatResources,
  GRAB,
  layoutBoard,
  Swatch,
  useDropTarget,
  WorkerCard,
  type Board,
  type Drag,
} from './placement-canvas';

// Width of the palette plus its margin, kept clear when the canvas fits its content.
const PALETTE_INSET = 288;

const FLOATING = 'oai:rounded-xl oai:border oai:bg-card oai:shadow-lg';

/** The floating box with every model; its cards are what gets dragged onto the canvas. */
function ModelPalette({
  models,
  placed,
  admin,
  drag,
  onDragModel,
  onDragEnd,
  onDropBack,
}: {
  models: readonly Model[];
  placed: (model: Model) => number;
  admin: boolean;
  drag: Drag | null;
  onDragModel: (model: Model) => void;
  onDragEnd: () => void;
  onDropBack: () => void;
}) {
  // Dragging a model off a node and back onto the palette takes it off that node.
  const returning = drag?.from !== undefined;
  const target = useDropTarget(returning, onDropBack);
  return (
    <aside
      aria-label="Models"
      className={`${FLOATING} oai:flex oai:max-h-[min(28rem,60vh)] oai:w-64 oai:flex-col oai:transition-[border-color,box-shadow] ${
        target.over ? 'oai:border-primary oai:ring-4 oai:ring-primary/15' : returning ? 'oai:border-primary/50' : ''
      }`}
      {...target.handlers}
    >
      <header className="oai:border-b oai:px-4 oai:py-3">
        <div className="oai:flex oai:items-center oai:gap-2">
          <h2 className="oai:text-sm oai:font-semibold">Models</h2>
          <Badge variant="secondary" className="oai:tabular-nums">
            {models.length}
          </Badge>
        </div>
        <p className="oai:mt-0.5 oai:text-xs oai:text-muted-foreground">
          {admin ? 'Drag a model onto a node, or use + on a node.' : 'Models that can be placed.'}
        </p>
      </header>
      {models.length === 0 ? (
        <p className="oai:p-4 oai:text-sm oai:text-muted-foreground">No models yet. Add one on the Models page.</p>
      ) : (
        <div className="oai:relative oai:flex oai:min-h-0 oai:flex-col">
          {/* Marked so the canvas leaves the wheel to this list instead of panning underneath. */}
          <ul data-canvas-scroll className="oai:min-h-0 oai:space-y-1.5 oai:overflow-y-auto oai:p-2">
            {models.map((model) => {
              const count = placed(model);
              const lifted = drag !== null && drag.from === undefined && drag.model.id === model.id;
              return (
                <li
                  key={model.id}
                  data-testid={`model-${model.name}`}
                  className={`oai:flex oai:items-center oai:gap-2.5 oai:rounded-lg oai:border oai:bg-background oai:py-2 oai:pr-2.5 oai:transition-[box-shadow,border-color,opacity] ${
                    admin ? `oai:pl-2 ${GRAB} oai:hover:border-foreground/20 oai:hover:shadow-sm` : 'oai:pl-3'
                  } ${lifted ? 'oai:opacity-40' : ''}`}
                  {...dragProps(admin, model.name, () => onDragModel(model), onDragEnd)}
                >
                  {admin ? (
                    <GripVerticalIcon aria-hidden className="oai:-mx-0.5 oai:size-4 oai:shrink-0 oai:text-muted-foreground/60" />
                  ) : null}
                  <Swatch modelId={model.id} className="oai:size-3" />
                  <div className="oai:min-w-0 oai:flex-1">
                    <div className="oai:truncate oai:text-sm oai:leading-5 oai:font-medium">{model.name}</div>
                    <div className="oai:truncate oai:font-mono oai:text-[11px] oai:leading-4 oai:text-muted-foreground">
                      {formatResources(model)}
                    </div>
                  </div>
                  <Badge
                    variant={count > 0 ? 'secondary' : 'outline'}
                    className={`oai:gap-1 oai:tabular-nums ${count > 0 ? '' : 'oai:text-muted-foreground'}`}
                    title={`On ${count} ${count === 1 ? 'node' : 'nodes'}`}
                  >
                    <ServerIcon aria-hidden />
                    {count}
                    <span className="oai:sr-only">{count === 1 ? ' node' : ' nodes'}</span>
                  </Badge>
                </li>
              );
            })}
          </ul>
          {returning ? (
            <div
              className={`oai:pointer-events-none oai:absolute oai:inset-2 oai:flex oai:items-center oai:justify-center oai:gap-2 oai:rounded-lg oai:border-2 oai:px-3 oai:text-center oai:text-sm oai:font-medium oai:text-primary oai:backdrop-blur-[1px] oai:transition-colors ${
                target.over ? 'oai:border-primary oai:bg-card/95' : 'oai:border-dashed oai:border-primary/50 oai:bg-card/90'
              }`}
            >
              <ArrowDownToLineIcon aria-hidden className="oai:size-4 oai:shrink-0" />
              <span className="oai:line-clamp-2">Drop to take {drag.model.name} off its node</span>
            </div>
          ) : null}
        </div>
      )}
    </aside>
  );
}

function DiffCount({ count, kind }: { count: number; kind: 'added' | 'removed' }) {
  if (count === 0) return null;
  const added = kind === 'added';
  const Icon = added ? PlusIcon : MinusIcon;
  return (
    <span
      className={`oai:inline-flex oai:items-center oai:gap-0.5 oai:rounded-md oai:px-1.5 oai:text-xs oai:leading-6 oai:font-semibold oai:tabular-nums ${
        added ? 'oai:bg-success/12 oai:text-success' : 'oai:bg-destructive/10 oai:text-destructive'
      }`}
    >
      <Icon aria-hidden className="oai:size-3" />
      {count}
      <span className="oai:sr-only">{added ? ' to deploy' : ' to remove'}</span>
    </span>
  );
}

/** Only shown while there is something to apply. */
function ApplyBar({
  plan,
  applying,
  onApply,
  onDiscard,
}: {
  plan: Plan;
  applying: boolean;
  onApply: () => void;
  onDiscard: () => void;
}) {
  const pending = planSize(plan) > 0;
  const visible = pending || applying;
  const summary = applying
    ? 'Applying changes'
    : pending
      ? [
          plan.added.length > 0 ? `${plan.added.length} to deploy` : '',
          plan.removed.length > 0 ? `${plan.removed.length} to remove` : '',
        ]
          .filter(Boolean)
          .join(', ')
      : 'No pending changes';
  return (
    <>
      {/* Outside the bar, which goes inert when hidden, so the change is still announced. */}
      <p role="status" className="oai:sr-only">
        {summary}
      </p>
      <div
        inert={!visible}
        className={`oai:absolute oai:bottom-4 oai:left-1/2 oai:-translate-x-1/2 oai:transition-[opacity,translate] oai:duration-200 ${
          visible ? 'oai:pointer-events-auto' : 'oai:pointer-events-none oai:translate-y-3 oai:opacity-0'
        }`}
      >
        <div className={`${FLOATING} oai:flex oai:items-center oai:gap-3 oai:py-1.5 oai:pr-1.5 oai:pl-4`}>
          <span className="oai:text-sm oai:font-medium oai:whitespace-nowrap">Staged changes</span>
          <span className="oai:flex oai:items-center oai:gap-1">
            <DiffCount kind="added" count={plan.added.length} />
            <DiffCount kind="removed" count={plan.removed.length} />
          </span>
          <div aria-hidden className="oai:h-5 oai:w-px oai:bg-border" />
          <span className="oai:flex oai:items-center oai:gap-1">
            <Button size="sm" variant="ghost" disabled={!pending || applying} onClick={onDiscard}>
              Discard
            </Button>
            <Button size="sm" disabled={!pending || applying} onClick={onApply}>
              {applying ? (
                <LoaderCircleIcon aria-hidden className="oai:animate-spin" />
              ) : (
                <CheckIcon aria-hidden />
              )}
              {applying ? 'Applying' : 'Apply'}
            </Button>
          </span>
        </div>
      </div>
    </>
  );
}

export function PlacementPage() {
  const nodesQuery = useNodes();
  const modelsQuery = useModels();
  const deploymentsQuery = useDeployments();
  const admin = useIsAdmin();
  const create = useCreateDeployment();
  const remove = useDeleteDeployment();
  const [plan, setPlan] = useState<Plan>(EMPTY_PLAN);
  const [drag, setDrag] = useState<Drag | null>(null);
  const [applying, setApplying] = useState(false);

  const nodes = useMemo(() => nodesQuery.data?.nodes ?? [], [nodesQuery.data]);
  const models = useMemo(() => modelsQuery.data ?? [], [modelsQuery.data]);
  const deployments = useMemo(() => deploymentsQuery.data ?? [], [deploymentsQuery.data]);
  const warnings = nodesQuery.data?.warnings ?? [];

  // The data refreshes underneath the plan; entries that lost their subject are dropped.
  useEffect(() => {
    if (deploymentsQuery.data && modelsQuery.data && nodesQuery.data) {
      setPlan((current) => prune(current, deployments, models, nodes));
    }
  }, [deployments, models, nodes, deploymentsQuery.data, modelsQuery.data, nodesQuery.data]);

  const layout = useMemo(
    () => layoutBoard(nodes, (node) => slotsOn(node, deployments, models, plan).length),
    [nodes, deployments, models, plan],
  );
  const editable = admin && !applying;
  const board = useMemo<Board>(
    () => ({
      admin: editable,
      models,
      drag,
      slotsOn: (node) => slotsOn(node, deployments, models, plan),
      onDrop: (target) => {
        if (!drag) return;
        const lifted = drag.from ? unassign(plan, drag.from.slot, drag.from.node) : plan;
        setPlan(assign(lifted, drag.model, target, deployments));
        setDrag(null);
      },
      onPlace: (node, model) => setPlan(assign(plan, model, node, deployments)),
      onDragSlot: (node, slot) => slot.model && setDrag({ model: slot.model, from: { node, slot } }),
      onDragEnd: () => setDrag(null),
      onUnassign: (node, slot) => setPlan(unassign(plan, slot, node)),
      onRestore: (deployment) => setPlan(restore(plan, deployment.id)),
    }),
    [editable, drag, deployments, models, plan],
  );
  const placed = (model: Model) =>
    nodes.reduce(
      (count, node: Node) =>
        count + board.slotsOn(node).filter((s) => s.kind !== 'removing' && s.model?.id === model.id).length,
      0,
    );

  // One at a time, removals first so their capacity is on its way back before anything new lands.
  // What fails stays in the plan (the shell toasts the reason) so it can be retried or dropped.
  const apply = async () => {
    setApplying(true);
    const failed: Plan = { added: [], removed: [] };
    for (const id of plan.removed) {
      const deployment = deployments.find((d) => d.id === id);
      if (!deployment) continue;
      await remove.mutateAsync(deployment).catch(() => failed.removed.push(id));
    }
    for (const a of plan.added) {
      await create
        .mutateAsync({ model_id: a.modelId, cluster: a.cluster, node: a.node })
        .catch(() => failed.added.push(a));
    }
    setPlan(failed);
    setApplying(false);
  };

  return (
    // Radix tooltips throw without a provider, and the page should not rely on the shell's. The
    // delay is longer than the shell's instant one because the board is dense with icon buttons
    // and tips would flicker on every pass of the pointer.
    <TooltipProvider delayDuration={300}>
      <div className="oai:space-y-4">
        <PageHeader
          title="Placement"
          description="Which model runs on which worker node. Changes are planned on the canvas and take effect when you apply them."
        />
        {warnings.length > 0 ? (
          <Alert variant="warning">
            <TriangleAlertIcon aria-hidden />
            <AlertTitle>The node list may be incomplete</AlertTitle>
            <AlertDescription>
              {warnings.length === 1 ? (
                <p>{warnings[0]}</p>
              ) : (
                <ul className="oai:list-disc oai:space-y-0.5 oai:pl-4">
                  {warnings.map((warning) => (
                    <li key={warning}>{warning}</li>
                  ))}
                </ul>
              )}
            </AlertDescription>
          </Alert>
        ) : null}
        {nodesQuery.isError ? (
          <QueryErrorAlert
            title="Could not load the nodes"
            error={nodesQuery.error}
            onRetry={() => void nodesQuery.refetch()}
          />
        ) : nodesQuery.isPending ? (
          <Skeleton className="oai:h-[32rem] oai:w-full oai:rounded-xl" />
        ) : nodes.length === 0 ? (
          <EmptyState icon={<ServerIcon />} title="No worker nodes" description="Oakestra reported no nodes." />
        ) : (
          <div className="oai:h-[calc(100dvh-15rem)] oai:min-h-[32rem] oai:overflow-hidden oai:rounded-xl oai:border oai:bg-muted/30">
            <BoardContext value={board}>
              <Canvas
                label="Placement canvas"
                contentWidth={layout.width}
                contentHeight={layout.height}
                insetLeft={PALETTE_INSET}
                overlay={
                  <>
                    <div className="oai:pointer-events-auto oai:absolute oai:top-4 oai:left-4">
                      <ModelPalette
                        models={models}
                        placed={placed}
                        admin={editable}
                        drag={drag}
                        onDragModel={(model) => setDrag({ model })}
                        onDragEnd={() => setDrag(null)}
                        onDropBack={() => {
                          if (drag?.from) setPlan(unassign(plan, drag.from.slot, drag.from.node));
                          setDrag(null);
                        }}
                      />
                    </div>
                    {admin ? (
                      <ApplyBar
                        plan={plan}
                        applying={applying}
                        onApply={() => void apply()}
                        onDiscard={() => setPlan(EMPTY_PLAN)}
                      />
                    ) : null}
                  </>
                }
              >
                {layout.items.map((item) => (
                  <div
                    key={item.kind === 'cluster' ? `cluster:${item.name}` : `${item.node.cluster}/${item.node.name}`}
                    className="oai:absolute"
                    style={{ left: item.x, top: item.y, width: item.width, height: item.height }}
                  >
                    {item.kind === 'cluster' ? (
                      <ClusterFrame name={item.name} workers={item.workers} />
                    ) : (
                      <WorkerCard node={item.node} />
                    )}
                  </div>
                ))}
              </Canvas>
            </BoardContext>
          </div>
        )}
      </div>
    </TooltipProvider>
  );
}
