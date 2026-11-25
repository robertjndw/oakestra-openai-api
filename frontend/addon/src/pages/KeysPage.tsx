import {
  Alert,
  AlertDescription,
  AlertTitle,
  Button,
  ConfirmDialog,
  CopyButton,
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  EmptyState,
  Input,
  Label,
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
import { KeyRoundIcon, PlusIcon } from 'lucide-react';
import { useState } from 'react';

import { useCreateKey, useDeleteKey, useIsAdmin, useKeys } from '../api';
import { formatTime } from '../lib';
import type { CreatedKey, Key } from '../types';

function CreateKeyDialog({ open, onOpenChange }: { open: boolean; onOpenChange: (o: boolean) => void }) {
  const [name, setName] = useState('');
  const [created, setCreated] = useState<CreatedKey | null>(null);
  const create = useCreateKey();

  function close(next: boolean) {
    onOpenChange(next);
    if (!next) {
      setName('');
      setCreated(null);
      create.reset();
    }
  }

  return (
    <Dialog open={open} onOpenChange={close}>
      <DialogContent>
        {created ? (
          <>
            <DialogHeader>
              <DialogTitle>Key {created.key.name} created</DialogTitle>
              <DialogDescription>Copy the secret now.</DialogDescription>
            </DialogHeader>
            <Alert>
              <AlertTitle>This secret will not be shown again</AlertTitle>
              <AlertDescription>
                Only a hash is stored. If you lose it, revoke the key and create a new one.
              </AlertDescription>
            </Alert>
            <div className="oai:flex oai:items-center oai:gap-2">
              <code
                data-testid="key-secret"
                className="oai:min-w-0 oai:flex-1 oai:break-all oai:rounded-md oai:bg-muted oai:px-3 oai:py-2 oai:text-sm"
              >
                {created.secret}
              </code>
              <CopyButton value={created.secret} label="Copy secret" variant="outline" />
            </div>
            <DialogFooter>
              <Button onClick={() => close(false)}>Done</Button>
            </DialogFooter>
          </>
        ) : (
          <form
            className="oai:space-y-4"
            onSubmit={(event) => {
              event.preventDefault();
              create.mutate(name.trim(), { onSuccess: setCreated });
            }}
          >
            <DialogHeader>
              <DialogTitle>Create API key</DialogTitle>
              <DialogDescription>Name it after the client that will use it.</DialogDescription>
            </DialogHeader>
            <div className="oai:space-y-2">
              <Label htmlFor="key-name">Name</Label>
              <Input
                id="key-name"
                value={name}
                onChange={(event) => setName(event.target.value)}
                placeholder="e.g. editor-plugin"
                required
              />
            </div>
            {create.isError ? (
              <p role="alert" className="oai:text-sm oai:text-destructive">
                {create.error.message}
              </p>
            ) : null}
            <DialogFooter>
              <Button type="submit" disabled={!name.trim() || create.isPending}>
                Create key
              </Button>
            </DialogFooter>
          </form>
        )}
      </DialogContent>
    </Dialog>
  );
}

export function KeysPage() {
  const keys = useKeys();
  const admin = useIsAdmin();
  const [creating, setCreating] = useState(false);
  const [revoking, setRevoking] = useState<Key | null>(null);
  const revoke = useDeleteKey();

  return (
    <div className="oai:space-y-6">
      <PageHeader
        title="API keys"
        description="Clients send a key as a bearer token to the OpenAI endpoint."
        actions={
          admin ? (
            <Button onClick={() => setCreating(true)}>
              <PlusIcon aria-hidden /> Create key
            </Button>
          ) : null
        }
      />
      {keys.isError ? (
        <QueryErrorAlert
          title="Could not load the keys"
          error={keys.error}
          onRetry={() => void keys.refetch()}
        />
      ) : keys.isPending ? (
        <Skeleton className="oai:h-32 oai:w-full" />
      ) : keys.data.length === 0 ? (
        <EmptyState
          icon={<KeyRoundIcon />}
          title="No API keys yet"
          description="Create a key to let a client call the endpoint."
        />
      ) : (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Name</TableHead>
              <TableHead>Key</TableHead>
              <TableHead>Created</TableHead>
              <TableHead>Last used</TableHead>
              <TableHead />
            </TableRow>
          </TableHeader>
          <TableBody>
            {keys.data.map((key) => (
              <TableRow key={key.id}>
                <TableCell className="oai:font-medium">{key.name}</TableCell>
                <TableCell>
                  <code>{key.prefix}...</code>
                </TableCell>
                <TableCell>{formatTime(key.created_at)}</TableCell>
                <TableCell>{formatTime(key.last_used_at)}</TableCell>
                <TableCell className="oai:text-right">
                  {admin ? (
                    <Button variant="outline" size="sm" onClick={() => setRevoking(key)}>
                      Revoke
                    </Button>
                  ) : null}
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}

      <CreateKeyDialog open={creating} onOpenChange={setCreating} />
      <ConfirmDialog
        open={revoking !== null}
        onOpenChange={(open) => !open && setRevoking(null)}
        title={`Revoke ${revoking?.name ?? 'key'}?`}
        description="Clients using this key stop working immediately."
        confirmLabel="Revoke"
        destructive
        pending={revoke.isPending}
        onConfirm={() => revoking && revoke.mutate(revoking, { onSettled: () => setRevoking(null) })}
      />
    </div>
  );
}
