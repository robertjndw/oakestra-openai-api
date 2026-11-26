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
  DropdownMenuItem,
  EmptyState,
  Input,
  Label,
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
import { KeyRoundIcon, PlusIcon, Trash2Icon, TriangleAlertIcon } from 'lucide-react';
import { useState } from 'react';

import { useCreateKey, useDeleteKey, useIsAdmin, useKeys } from '../api';
import { formatRelative, formatTime } from '../lib';
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
            <Alert variant="warning">
              <TriangleAlertIcon aria-hidden />
              <AlertTitle>This secret will not be shown again</AlertTitle>
              <AlertDescription>
                Only a hash is stored. If you lose it, revoke the key and create a new one.
              </AlertDescription>
            </Alert>
            <div className="oai:flex oai:items-start oai:gap-2 oai:rounded-md oai:border oai:bg-muted oai:p-2 oai:pl-3">
              <code
                data-testid="key-secret"
                className="oai:min-w-0 oai:flex-1 oai:py-1 oai:font-mono oai:text-sm oai:break-all oai:select-all"
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
                autoFocus
                autoComplete="off"
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
                {create.isPending ? 'Creating...' : 'Create key'}
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
          admin && keys.data?.length ? (
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
          action={
            admin ? (
              <Button onClick={() => setCreating(true)}>
                <PlusIcon aria-hidden /> Create key
              </Button>
            ) : undefined
          }
        />
      ) : (
        <div className="oai:overflow-hidden oai:rounded-lg oai:border oai:bg-card">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Name</TableHead>
                <TableHead>Key</TableHead>
                <TableHead>Created</TableHead>
                <TableHead>Last used</TableHead>
                <TableHead className="oai:w-10">
                  <span className="oai:sr-only">Actions</span>
                </TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {keys.data.map((key) => (
                <TableRow key={key.id}>
                  <TableCell className="oai:font-medium">{key.name}</TableCell>
                  <TableCell>
                    <code className="oai:font-mono oai:text-xs oai:text-muted-foreground">{key.prefix}...</code>
                  </TableCell>
                  <TableCell className="oai:text-muted-foreground">
                    <time dateTime={new Date(key.created_at).toISOString()} title={formatTime(key.created_at)}>
                      {formatRelative(key.created_at)}
                    </time>
                  </TableCell>
                  <TableCell className="oai:text-muted-foreground">
                    <span title={formatTime(key.last_used_at)}>{formatRelative(key.last_used_at)}</span>
                  </TableCell>
                  <TableCell className="oai:text-right">
                    {admin ? (
                      <RowMenu label={`Actions for ${key.name}`}>
                        <DropdownMenuItem variant="destructive" onSelect={() => setRevoking(key)}>
                          <Trash2Icon aria-hidden /> Revoke
                        </DropdownMenuItem>
                      </RowMenu>
                    ) : null}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </div>
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
