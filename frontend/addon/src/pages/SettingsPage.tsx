import {
  Button,
  Label,
  PageHeader,
  QueryErrorAlert,
  RadioGroup,
  RadioGroupItem,
  SectionCard,
  Skeleton,
} from '@oakestra/ui';
import { CheckIcon } from 'lucide-react';
import { useState } from 'react';

import { useSaveSettings, useSettings } from '../api';

/** Mirrors the strategies registered in internal/scheduler; unknown names get a generic line. */
const STRATEGY_DESCRIPTIONS: Record<string, string> = {
  'least-inflight':
    'Sends each request to the ready deployment with the fewest requests in flight, then the lowest known time to first token.',
};

const FALLBACK_DESCRIPTION = 'A custom strategy provided by this backend.';

export function SettingsPage() {
  const settings = useSettings();
  const save = useSaveSettings();
  // Null until the person changes it, so the radio group follows the server value until then.
  const [choice, setChoice] = useState<string | null>(null);
  const [saved, setSaved] = useState(false);

  const data = settings.data;
  const strategy = choice ?? data?.scheduler.strategy ?? '';
  const dirty = data !== undefined && strategy !== data.scheduler.strategy;

  return (
    <div className="oai:space-y-6">
      <PageHeader
        title="Settings"
        description="Control how requests for the model auto are spread across your deployments."
      />
      {settings.isError ? (
        <QueryErrorAlert
          title="Could not load the settings"
          error={settings.error}
          onRetry={() => void settings.refetch()}
        />
      ) : !data ? (
        <Skeleton className="oai:h-48 oai:w-full" />
      ) : (
        <form
          onSubmit={(event) => {
            event.preventDefault();
            // The strategy's own config is kept as the backend returned it.
            save.mutate(
              { strategy, config: data.scheduler.config },
              {
                onSuccess: () => {
                  setChoice(null);
                  setSaved(true);
                },
              },
            );
          }}
        >
          <SectionCard
            title="Scheduler"
            description='Decides which ready model serves a request for model "auto".'
            footer={
              <>
                <Button type="submit" disabled={!dirty || save.isPending}>
                  {save.isPending ? 'Saving...' : 'Save'}
                </Button>
                {saved && !dirty && !save.isPending ? (
                  <span
                    role="status"
                    className="oai:inline-flex oai:items-center oai:gap-1 oai:text-sm oai:text-muted-foreground"
                  >
                    <CheckIcon aria-hidden className="oai:size-4 oai:text-success" /> Saved
                  </span>
                ) : null}
              </>
            }
          >
            <RadioGroup
              aria-label="Strategy"
              value={strategy}
              onValueChange={(next) => {
                setChoice(next);
                setSaved(false);
              }}
              disabled={save.isPending}
            >
              {data.strategies.map((name) => (
                <div
                  key={name}
                  className="oai:flex oai:items-start oai:gap-3 oai:rounded-md oai:border oai:p-3 oai:transition-colors oai:has-data-[state=checked]:border-primary oai:has-data-[state=checked]:bg-accent/40"
                >
                  <RadioGroupItem
                    id={`strategy-${name}`}
                    value={name}
                    aria-describedby={`strategy-${name}-description`}
                    className="oai:mt-0.5"
                  />
                  <div className="oai:min-w-0 oai:space-y-1">
                    <Label htmlFor={`strategy-${name}`} className="oai:font-mono oai:text-sm">
                      {name}
                    </Label>
                    <p
                      id={`strategy-${name}-description`}
                      className="oai:text-sm oai:text-muted-foreground"
                    >
                      {STRATEGY_DESCRIPTIONS[name] ?? FALLBACK_DESCRIPTION}
                    </p>
                  </div>
                </div>
              ))}
            </RadioGroup>
          </SectionCard>
        </form>
      )}
    </div>
  );
}
