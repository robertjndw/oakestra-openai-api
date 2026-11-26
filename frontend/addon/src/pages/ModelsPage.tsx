import {
  Badge,
  Button,
  ConfirmDialog,
  CopyButton,
  DropdownMenuItem,
  DropdownMenuSeparator,
  EmptyState,
  PageHeader,
  QueryErrorAlert,
  RowMenu,
  Skeleton,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@oakestra/ui';
import { BoxIcon, GpuIcon, MemoryStickIcon, PencilIcon, PlusIcon, Trash2Icon } from 'lucide-react';
import { useState } from 'react';

import { useDeleteModel, useDeployments, useIsAdmin, useModels } from '../api';
import { formatMb } from '../lib';
import type { Model } from '../types';
import { ModelDialog } from './ModelDialog';

const surface = 'oai:overflow-hidden oai:rounded-lg oai:border oai:bg-card';

function Resources({ model }: { model: Model }) {
  return (
    <span className="oai:inline-flex oai:items-center oai:gap-3 oai:whitespace-nowrap oai:tabular-nums">
      <span className="oai:inline-flex oai:items-center oai:gap-1" title="Memory">
        <MemoryStickIcon aria-hidden className="oai:size-3.5 oai:text-muted-foreground" />
        {formatMb(model.memory_mb)}
      </span>
      <span
        className={
          model.vgpus > 0
            ? 'oai:inline-flex oai:items-center oai:gap-1'
            : 'oai:inline-flex oai:items-center oai:gap-1 oai:text-muted-foreground'
        }
        title="GPUs"
      >
        <GpuIcon aria-hidden className="oai:size-3.5 oai:text-muted-foreground" />
        {model.vgpus > 0 ? `${model.vgpus} GPU` : 'No GPU'}
      </span>
    </span>
  );
}

export function ModelsPage() {
  const models = useModels();
  const deployments = useDeployments();
  const admin = useIsAdmin();
  // `true` is the create dialog; a model edits that one.
  const [editing, setEditing] = useState<Model | true | null>(null);
  const [deleting, setDeleting] = useState<Model | null>(null);
  const remove = useDeleteModel();

  const deploymentCount = (model: Model) =>
    deployments.data?.filter((d) => d.model_id === model.id).length;

  const addButton = (
    <Button onClick={() => setEditing(true)}>
      <PlusIcon aria-hidden /> Add model
    </Button>
  );

  return (
    <div className="oai:space-y-6">
      <PageHeader
        title="Models"
        description="LLMs that can be placed on worker nodes and served through the endpoint."
        actions={admin && models.data?.length ? addButton : null}
      />
      {models.isError ? (
        <QueryErrorAlert
          title="Could not load the models"
          error={models.error}
          onRetry={() => void models.refetch()}
        />
      ) : models.isPending ? (
        <div className={`${surface} oai:space-y-3 oai:p-4`} aria-busy>
          <Skeleton className="oai:h-6 oai:w-1/3" />
          <Skeleton className="oai:h-6 oai:w-full" />
          <Skeleton className="oai:h-6 oai:w-full" />
        </div>
      ) : models.data.length === 0 ? (
        <EmptyState
          className="oai:rounded-lg oai:border oai:border-dashed"
          icon={<BoxIcon />}
          title="No models yet"
          description={
            admin
              ? 'A model is a ModelPack reference plus the engine that serves it. Add one, then place it on a node on the Placement page.'
              : 'An admin has not added any models yet.'
          }
          action={admin ? addButton : undefined}
        />
      ) : (
        <div className={surface}>
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead className="oai:pl-4">Name</TableHead>
                <TableHead>Reference</TableHead>
                <TableHead>Resources</TableHead>
                <TableHead className="oai:text-right">Deployments</TableHead>
                {admin ? (
                  <TableHead className="oai:w-12">
                    <span className="oai:sr-only">Actions</span>
                  </TableHead>
                ) : null}
              </TableRow>
            </TableHeader>
            <TableBody>
              {models.data.map((model) => {
                const count = deploymentCount(model);
                return (
                  <TableRow key={model.id}>
                    <TableCell className="oai:pl-4">
                      <span className="oai:flex oai:items-center oai:gap-2">
                        <span className="oai:font-medium">{model.name}</span>
                        {model.auto ? (
                          <Badge
                            variant="secondary"
                            title='The scheduler may pick this model for "model": "auto"'
                          >
                            auto
                          </Badge>
                        ) : null}
                      </span>
                    </TableCell>
                    <TableCell>
                      <span className="oai:flex oai:max-w-80 oai:items-center oai:gap-1">
                        <code
                          className="oai:min-w-0 oai:truncate oai:font-mono oai:text-xs oai:text-muted-foreground"
                          title={model.reference}
                        >
                          {model.reference}
                        </code>
                        <CopyButton
                          value={model.reference}
                          label={`Copy the reference of ${model.name}`}
                          size="icon-xs"
                          className="oai:shrink-0"
                        />
                      </span>
                    </TableCell>
                    <TableCell>
                      <Resources model={model} />
                    </TableCell>
                    <TableCell
                      className={
                        count
                          ? 'oai:text-right oai:tabular-nums'
                          : 'oai:text-right oai:tabular-nums oai:text-muted-foreground'
                      }
                    >
                      {count ?? '-'}
                    </TableCell>
                    {admin ? (
                      <TableCell className="oai:pr-2 oai:text-right">
                        <RowMenu label={`Actions for ${model.name}`}>
                          <DropdownMenuItem onSelect={() => setEditing(model)}>
                            <PencilIcon aria-hidden />
                            Edit
                          </DropdownMenuItem>
                          <DropdownMenuSeparator />
                          <DropdownMenuItem variant="destructive" onSelect={() => setDeleting(model)}>
                            <Trash2Icon aria-hidden />
                            Delete
                          </DropdownMenuItem>
                        </RowMenu>
                      </TableCell>
                    ) : null}
                  </TableRow>
                );
              })}
            </TableBody>
          </Table>
        </div>
      )}

      {editing !== null ? (
        <ModelDialog
          key={editing === true ? 'new' : editing.id}
          model={editing === true ? undefined : editing}
          onClose={() => setEditing(null)}
        />
      ) : null}
      <ConfirmDialog
        open={deleting !== null}
        onOpenChange={(open) => !open && setDeleting(null)}
        title={`Delete ${deleting?.name ?? 'model'}?`}
        description={
          deleting && deploymentCount(deleting)
            ? 'This model still has deployments, so the backend will refuse. Remove them on the Placement page first.'
            : 'The model definition is removed. Clients can no longer request it.'
        }
        confirmLabel="Delete"
        destructive
        pending={remove.isPending}
        onConfirm={() => deleting && remove.mutate(deleting, { onSettled: () => setDeleting(null) })}
      />
    </div>
  );
}
