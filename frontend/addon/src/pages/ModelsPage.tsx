import {
  Badge,
  Button,
  ConfirmDialog,
  EmptyState,
  PageHeader,
  QueryErrorAlert,
  Skeleton,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@oakestra/ui';
import { BoxIcon, PlusIcon } from 'lucide-react';
import { useState } from 'react';

import { useDeleteModel, useDeployments, useIsAdmin, useModels } from '../api';
import { formatMb } from '../lib';
import type { Model } from '../types';
import { ModelDialog } from './ModelDialog';

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

  return (
    <div className="oai:space-y-6">
      <PageHeader
        title="Models"
        description="LLMs that can be placed on worker nodes and served through the endpoint."
        actions={
          admin ? (
            <Button onClick={() => setEditing(true)}>
              <PlusIcon aria-hidden /> Add model
            </Button>
          ) : null
        }
      />
      {models.isError ? (
        <QueryErrorAlert
          title="Could not load the models"
          error={models.error}
          onRetry={() => void models.refetch()}
        />
      ) : models.isPending ? (
        <Skeleton className="oai:h-32 oai:w-full" />
      ) : models.data.length === 0 ? (
        <EmptyState
          icon={<BoxIcon />}
          title="No models yet"
          description="Add a ModelPack reference to get started."
        />
      ) : (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Name</TableHead>
              <TableHead>Reference</TableHead>
              <TableHead>Resources</TableHead>
              <TableHead>Deployments</TableHead>
              <TableHead />
            </TableRow>
          </TableHeader>
          <TableBody>
            {models.data.map((model) => (
              <TableRow key={model.id}>
                <TableCell className="oai:font-medium">
                  {model.name}
                  {model.auto ? (
                    <Badge variant="secondary" className="oai:ml-2">
                      auto
                    </Badge>
                  ) : null}
                </TableCell>
                <TableCell className="oai:max-w-80 oai:truncate" title={model.reference}>
                  <code className="oai:text-xs">{model.reference}</code>
                </TableCell>
                <TableCell>
                  {formatMb(model.memory_mb)}, {model.vgpus} GPU
                </TableCell>
                <TableCell>{deploymentCount(model) ?? '-'}</TableCell>
                <TableCell className="oai:space-x-2 oai:text-right">
                  {admin ? (
                    <>
                      <Button variant="outline" size="sm" onClick={() => setEditing(model)}>
                        Edit
                      </Button>
                      <Button variant="outline" size="sm" onClick={() => setDeleting(model)}>
                        Delete
                      </Button>
                    </>
                  ) : null}
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
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
        description="The backend refuses while the model still has deployments; remove them on the Placement page first."
        confirmLabel="Delete"
        destructive
        pending={remove.isPending}
        onConfirm={() => deleting && remove.mutate(deleting, { onSettled: () => setDeleting(null) })}
      />
    </div>
  );
}
