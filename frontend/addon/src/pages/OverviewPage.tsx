import { CopyButton, PageHeader, QueryErrorAlert, SectionCard, StatCard, StatGroup } from '@oakestra/ui';

import { useOverview } from '../api';

function curlSnippet(publicUrl: string): string {
  return `curl ${publicUrl.replace(/\/$/, '')}/v1/chat/completions \\
  -H "Authorization: Bearer <YOUR_API_KEY>" \\
  -H "Content-Type: application/json" \\
  -d '{
    "model": "auto",
    "messages": [{"role": "user", "content": "Hello!"}]
  }'`;
}

export function OverviewPage() {
  const overview = useOverview();
  const data = overview.data;

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

      <StatGroup>
        <StatCard label="Models" value={data?.models} loading={!data} />
        <StatCard
          label="Deployments"
          value={data ? `${data.deployments.ready} / ${data.deployments.total}` : undefined}
          hint="ready / total"
          loading={!data}
        />
        <StatCard label="API keys" value={data?.keys} loading={!data} />
        <StatCard
          label="Oakestra"
          value={data?.oakestra.reachable ? 'Reachable' : 'Unreachable'}
          hint={data?.oakestra.reachable ? undefined : data?.oakestra.error}
          tone={data && !data.oakestra.reachable ? 'destructive' : 'default'}
          loading={!data}
        />
        <StatCard label="Scheduler" value={data?.scheduler} hint="strategy for auto" loading={!data} />
      </StatGroup>

      {data ? (
        <>
          <SectionCard title="Endpoint" description="Point any OpenAI client at this base URL.">
            <div className="oai:flex oai:items-center oai:gap-2">
              <code className="oai:min-w-0 oai:flex-1 oai:truncate oai:rounded-md oai:bg-muted oai:px-3 oai:py-2 oai:text-sm">
                {data.public_url}
              </code>
              <CopyButton value={data.public_url} label="Copy endpoint URL" variant="outline" />
            </div>
          </SectionCard>
          <SectionCard
            title="Try it"
            description='Model "auto" lets the scheduler pick a ready model. Create a key on the API keys page.'
            action={<CopyButton value={curlSnippet(data.public_url)} label="Copy snippet" text="Copy" />}
          >
            <pre className="oai:overflow-x-auto oai:rounded-md oai:bg-muted oai:p-3 oai:text-xs">
              {curlSnippet(data.public_url)}
            </pre>
          </SectionCard>
          <p className="oai:text-xs oai:text-muted-foreground">Backend version {data.version}</p>
        </>
      ) : null}
    </div>
  );
}
