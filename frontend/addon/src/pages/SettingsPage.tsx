import {
  Button,
  PageHeader,
  QueryErrorAlert,
  SectionCard,
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
  Skeleton,
  Label,
} from '@oakestra/ui';
import { useState } from 'react';

import { useSaveSettings, useSettings } from '../api';

export function SettingsPage() {
  const settings = useSettings();
  const save = useSaveSettings();
  // Null until the person changes it, so the select follows the server value until then.
  const [choice, setChoice] = useState<string | null>(null);

  const data = settings.data;
  const strategy = choice ?? data?.scheduler.strategy ?? '';
  const dirty = data !== undefined && strategy !== data.scheduler.strategy;

  return (
    <div className="oai:space-y-6">
      <PageHeader title="Settings" />
      {settings.isError ? (
        <QueryErrorAlert
          title="Could not load the settings"
          error={settings.error}
          onRetry={() => void settings.refetch()}
        />
      ) : !data ? (
        <Skeleton className="oai:h-32 oai:w-full" />
      ) : (
        <SectionCard
          title="Scheduler"
          description='Decides which ready model serves a request for model "auto".'
        >
          <form
            className="oai:flex oai:flex-wrap oai:items-end oai:gap-3"
            onSubmit={(event) => {
              event.preventDefault();
              // The strategy's own config is kept as the backend returned it.
              save.mutate(
                { strategy, config: data.scheduler.config },
                { onSuccess: () => setChoice(null) },
              );
            }}
          >
            <div className="oai:space-y-2">
              <Label id="strategy-label">Strategy</Label>
              <Select value={strategy} onValueChange={setChoice}>
                <SelectTrigger aria-labelledby="strategy-label" className="oai:w-64">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {data.strategies.map((name) => (
                    <SelectItem key={name} value={name}>
                      {name}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
            <Button type="submit" disabled={!dirty || save.isPending}>
              Save
            </Button>
          </form>
        </SectionCard>
      )}
    </div>
  );
}
