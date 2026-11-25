import {
  Alert,
  AlertDescription,
  AlertTitle,
  Button,
  EmptyState,
  PageHeader,
  QueryErrorAlert,
  Skeleton,
} from '@oakestra/ui';
import { GripVerticalIcon, ServerIcon } from 'lucide-react';
import { useEffect, useMemo, useState } from 'react';

import {
  useCreateDeployment,
  useDeleteDeployment,
  useDeployments,
  useIsAdmin,
  useModels,
  useNodes,
} from '../api';
import { formatMb } from '../lib';
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
  GRAB,
  layoutBoard,
  modelColor,
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
      className={`${FLOATING} oai:flex oai:max-h-[min(28rem,60vh)] oai:w-64 oai:flex-col oai:transition-colors ${
        target.over ? 'oai:border-primary oai:ring-2 oai:ring-primary' : returning ? 'oai:border-primary' : ''
      }`}
      {...target.handlers}
    >
      <header className="oai:border-b oai:px-4 oai:py-3">
        <h2 className="oai:text-sm oai:font-medium">Models</h2>
        <p className="oai:text-xs oai:text-muted-foreground">
          {returning
            ? `Drop here to take ${drag.model.name} off its node`
            : admin
              ? 'Drag a model onto a node'
              : 'Models that can be placed'}
        </p>
      </header>
      {models.length === 0 ? (
        <p className="oai:p-4 oai:text-sm oai:text-muted-foreground">No models yet. Add one on the Models page.</p>
      ) : (
        // Marked so the canvas leaves the wheel to this list instead of panning underneath.
        <ul data-canvas-scroll className="oai:space-y-2 oai:overflow-y-auto oai:p-3">
          {models.map((model) => (
            <li
              key={model.id}
              data-testid={`model-${model.name}`}
              className={`oai:flex oai:items-center oai:gap-2 oai:rounded-md oai:border oai:bg-background oai:py-2 oai:pr-3 oai:pl-2 oai:transition-shadow ${
                admin ? `${GRAB} oai:hover:shadow-md` : ''
              }`}
              style={{ borderLeftColor: modelColor(model.id), borderLeftWidth: 4 }}
              {...dragProps(admin, model.name, () => onDragModel(model), onDragEnd)}
            >
              {admin ? (
                <GripVerticalIcon aria-hidden className="oai:size-4 oai:shrink-0 oai:text-muted-foreground" />
              ) : null}
              <div className="oai:min-w-0">
                <div className="oai:truncate oai:text-sm oai:font-medium">{model.name}</div>
                <div className="oai:truncate oai:text-xs oai:text-muted-foreground">
                  {formatMb(model.memory_mb)}, {model.vgpus} GPU, on {placed(model)}{' '}
                  {placed(model) === 1 ? 'node' : 'nodes'}
                </div>
              </div>
            </li>
          ))}
        </ul>
      )}
    </aside>
  );
}

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
  const parts = [
    plan.added.length > 0 ? `${plan.added.length} to deploy` : '',
    plan.removed.length > 0 ? `${plan.removed.length} to remove` : '',
  ].filter(Boolean);
  const pending = planSize(plan) > 0;
  return (
    <div className={`${FLOATING} oai:flex oai:items-center oai:gap-3 oai:py-2 oai:pr-2 oai:pl-4`}>
      <p className="oai:text-sm" role="status">
        {pending ? `Pending: ${parts.join(', ')}` : 'No pending changes'}
      </p>
      <Button size="sm" variant="ghost" disabled={!pending || applying} onClick={onDiscard}>
        Discard
      </Button>
      <Button size="sm" disabled={!pending || applying} onClick={onApply}>
        {applying ? 'Applying...' : 'Apply'}
      </Button>
    </div>
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
      drag,
      slotsOn: (node) => slotsOn(node, deployments, models, plan),
      onDrop: (target) => {
        if (!drag) return;
        const lifted = drag.from ? unassign(plan, drag.from.slot, drag.from.node) : plan;
        setPlan(assign(lifted, drag.model, target, deployments));
        setDrag(null);
      },
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
    <div className="oai:space-y-4">
      <PageHeader
        title="Placement"
        description="Which model runs on which worker node. Changes are planned on the canvas and take effect when you apply them."
      />
      {nodesQuery.data?.warnings.map((warning) => (
        <Alert key={warning}>
          <AlertTitle>Warning</AlertTitle>
          <AlertDescription>{warning}</AlertDescription>
        </Alert>
      ))}
      {nodesQuery.isError ? (
        <QueryErrorAlert
          title="Could not load the nodes"
          error={nodesQuery.error}
          onRetry={() => void nodesQuery.refetch()}
        />
      ) : nodesQuery.isPending ? (
        <Skeleton className="oai:h-[32rem] oai:w-full" />
      ) : nodes.length === 0 ? (
        <EmptyState icon={<ServerIcon />} title="No worker nodes" description="Oakestra reported no nodes." />
      ) : (
        <div className="oai:h-[calc(100dvh-15rem)] oai:min-h-[32rem] oai:overflow-hidden oai:rounded-xl oai:border oai:bg-muted/20">
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
                    <div className="oai:pointer-events-auto oai:absolute oai:bottom-4 oai:left-1/2 oai:-translate-x-1/2">
                      <ApplyBar
                        plan={plan}
                        applying={applying}
                        onApply={() => void apply()}
                        onDiscard={() => setPlan(EMPTY_PLAN)}
                      />
                    </div>
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
  );
}
