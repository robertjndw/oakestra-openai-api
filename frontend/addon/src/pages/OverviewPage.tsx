import { useAddonPath } from '@oakestra/dashboard-sdk';
import {
  Alert,
  AlertDescription,
  AlertTitle,
  Button,
  CopyButton,
  PageHeader,
  QueryErrorAlert,
  SectionCard,
  Skeleton,
  StatCard,
  StatGroup,
  StatusBadge,
  Tabs,
  TabsContent,
  TabsList,
  TabsTrigger,
} from '@oakestra/ui';
import { ArrowRightIcon, LightbulbIcon } from 'lucide-react';
import { Link } from 'react-router';

import { useIsAdmin, useOverview } from '../api';
import type { Overview } from '../types';

/** What an OpenAI client takes as its base URL: the public URL plus the gateway's /v1 root. */
function baseUrl(publicUrl: string): string {
  return `${publicUrl.replace(/\/$/, '')}/v1`;
}

function curlSnippet(publicUrl: string): string {
  return `curl ${baseUrl(publicUrl)}/chat/completions \\
  -H "Authorization: Bearer <YOUR_API_KEY>" \\
  -H "Content-Type: application/json" \\
  -d '{
    "model": "auto",
    "messages": [{"role": "user", "content": "Hello!"}]
  }'`;
}

function pythonSnippet(publicUrl: string): string {
  return `from openai import OpenAI

client = OpenAI(
    base_url="${baseUrl(publicUrl)}",
    api_key="<YOUR_API_KEY>",
)

response = client.chat.completions.create(
    model="auto",
    messages=[{"role": "user", "content": "Hello!"}],
)
print(response.choices[0].message.content)`;
}

function javascriptSnippet(publicUrl: string): string {
  return `import OpenAI from "openai";

const client = new OpenAI({
  baseURL: "${baseUrl(publicUrl)}",
  apiKey: "<YOUR_API_KEY>",
});

const response = await client.chat.completions.create({
  model: "auto",
  messages: [{ role: "user", content: "Hello!" }],
});
console.log(response.choices[0].message.content);`;
}

const SNIPPETS = [
  { id: 'curl', label: 'curl', build: curlSnippet },
  { id: 'python', label: 'Python', build: pythonSnippet },
  { id: 'javascript', label: 'JavaScript', build: javascriptSnippet },
] as const;

function SnippetBlock({ code, label }: { code: string; label: string }) {
  return (
    <div className="oai:relative">
      <pre className="oai:overflow-x-auto oai:rounded-md oai:bg-muted oai:p-3 oai:pr-12 oai:font-mono oai:text-xs">
        {code}
      </pre>
      <CopyButton
        value={code}
        label={`Copy ${label} snippet`}
        className="oai:absolute oai:top-1.5 oai:right-1.5"
      />
    </div>
  );
}

function SnippetTabs({ publicUrl }: { publicUrl: string }) {
  return (
    <Tabs defaultValue="curl">
      <TabsList>
        {SNIPPETS.map((s) => (
          <TabsTrigger key={s.id} value={s.id}>
            {s.label}
          </TabsTrigger>
        ))}
      </TabsList>
      {SNIPPETS.map((s) => (
        <TabsContent key={s.id} value={s.id} className="oai:pt-2">
          <SnippetBlock code={s.build(publicUrl)} label={s.label} />
        </TabsContent>
      ))}
    </Tabs>
  );
}

interface NextStep {
  title: string;
  text: string;
  to: string;
  action: string;
}

/** The first thing missing before a request can succeed; null once the basics are in place. */
function nextStep(data: Overview, admin: boolean, path: (...s: string[]) => string): NextStep | null {
  if (data.models === 0) {
    return {
      title: 'Add your first model',
      text: 'Define a model to serve, then place it on a worker.',
      to: path('models'),
      action: 'Go to Models',
    };
  }
  if (data.deployments.ready === 0) {
    return {
      title: 'Place a model on a worker',
      text: 'No deployment is ready yet, so requests have nothing to run on.',
      to: path('placement'),
      action: 'Go to Placement',
    };
  }
  if (data.keys === 0 && admin) {
    return {
      title: 'Create an API key',
      text: 'Clients authenticate to the endpoint with a bearer key.',
      to: path('keys'),
      action: 'Go to API keys',
    };
  }
  return null;
}

/** Unlinked cards (undefined) render as plain tiles, e.g. admin-only pages for other users. */
const statLink = (to: string | undefined) => (to ? <Link to={to} /> : undefined);

export function OverviewPage() {
  const overview = useOverview();
  const admin = useIsAdmin();
  const path = useAddonPath();
  const data = overview.data;
  const loading = !data;
  const step = data ? nextStep(data, admin, path) : null;
  const { deployments } = data ?? { deployments: { ready: 0, total: 0 } };

  return (
    <div className="oai:space-y-6">
      <PageHeader
        title="OpenAI API"
        description="An OpenAI-compatible endpoint backed by models running on your Oakestra workers."
      />
      {overview.isError ? (
        <QueryErrorAlert
          title="Could not load the overview"
          error={overview.error}
          onRetry={() => void overview.refetch()}
        />
      ) : null}

      {step ? (
        <Alert variant="info">
          <LightbulbIcon aria-hidden />
          <AlertTitle>{step.title}</AlertTitle>
          <AlertDescription>
            <p>{step.text}</p>
            <Button asChild variant="link" size="sm" className="oai:mt-1 oai:h-auto oai:p-0">
              <Link to={step.to}>
                {step.action} <ArrowRightIcon aria-hidden />
              </Link>
            </Button>
          </AlertDescription>
        </Alert>
      ) : null}

      <StatGroup>
        <StatCard label="Models" value={data?.models} loading={loading} asChild>
          {statLink(path('models'))}
        </StatCard>
        <StatCard
          label="Deployments"
          value={data ? `${deployments.ready} of ${deployments.total}` : undefined}
          hint="ready"
          tone={data && deployments.total > 0 && deployments.ready === 0 ? 'warning' : 'default'}
          loading={loading}
          asChild
        >
          {statLink(path('placement'))}
        </StatCard>
        <StatCard label="API keys" value={data?.keys} loading={loading} asChild={admin}>
          {statLink(admin ? path('keys') : undefined)}
        </StatCard>
        <StatCard
          label="Oakestra"
          value={
            data ? (
              <span className="oai:flex oai:h-9 oai:items-center">
                <StatusBadge tone={data.oakestra.reachable ? 'success' : 'destructive'}>
                  {data.oakestra.reachable ? 'Reachable' : 'Unreachable'}
                </StatusBadge>
              </span>
            ) : undefined
          }
          hint={data?.oakestra.reachable ? 'control plane' : (data?.oakestra.error ?? 'control plane')}
          loading={loading}
        />
        <StatCard
          label="Scheduler"
          value={data?.scheduler}
          hint="strategy for auto"
          loading={loading}
          asChild={admin}
        >
          {statLink(admin ? path('settings') : undefined)}
        </StatCard>
      </StatGroup>

      <SectionCard title="Endpoint" description="Point any OpenAI client at this base URL.">
        {data ? (
          <div className="oai:flex oai:items-center oai:gap-2">
            <code className="oai:min-w-0 oai:flex-1 oai:truncate oai:rounded-md oai:bg-muted oai:px-3 oai:py-2 oai:font-mono oai:text-sm">
              {baseUrl(data.public_url)}
            </code>
            <CopyButton value={baseUrl(data.public_url)} label="Copy endpoint URL" variant="outline" />
          </div>
        ) : (
          <Skeleton className="oai:h-9 oai:w-full" />
        )}
      </SectionCard>

      <SectionCard
        title="Try it"
        description='Model "auto" lets the scheduler pick a ready model. Create a key on the API keys page.'
      >
        {data ? <SnippetTabs publicUrl={data.public_url} /> : <Skeleton className="oai:h-48 oai:w-full" />}
      </SectionCard>

      {data ? (
        <p className="oai:text-center oai:text-xs oai:text-muted-foreground">
          Backend version {data.version}
        </p>
      ) : null}
    </div>
  );
}
