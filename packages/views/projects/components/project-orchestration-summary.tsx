"use client";

import { useQuery } from "@tanstack/react-query";
import { projectOrchestrationOptions } from "@multica/core/projects/queries";
import type { IssueOrchestrationSummary, ProjectOrchestrationSummary } from "@multica/core/types";
import { Button } from "@multica/ui/components/ui/button";
import { Skeleton } from "@multica/ui/components/ui/skeleton";
import { cn } from "@multica/ui/lib/utils";
import { AlertCircle, CheckCircle2, Clock3, LoaderCircle, RefreshCw, Sparkles } from "lucide-react";
import { useT } from "../../i18n";

const tone: Record<ProjectOrchestrationSummary["classification"], string> = {
  ready: "border-blue-500/25 bg-blue-500/5",
  complete: "border-emerald-500/25 bg-emerald-500/5",
  waiting_external: "border-amber-500/25 bg-amber-500/5",
  temporarily_not_ready: "border-border bg-muted/30",
  orchestration_fault: "border-destructive/30 bg-destructive/5",
};

function StateIcon({ state }: { state: IssueOrchestrationSummary["execution_state"] }) {
  if (state === "running") return <LoaderCircle className="size-4 animate-spin text-blue-600 motion-reduce:animate-none" />;
  if (state === "complete") return <CheckCircle2 className="size-4 text-emerald-600" />;
  if (state === "faulted") return <AlertCircle className="size-4 text-destructive" />;
  return <Clock3 className="size-4 text-muted-foreground" />;
}

export function ProjectOrchestrationSummaryCard({ projectId, wsId }: { projectId: string; wsId: string }) {
  const { t } = useT("projects");
  const query = useQuery(projectOrchestrationOptions(wsId, projectId));
  const summary = query.data;

  if (query.isLoading) {
    return <div className="border-b p-4" aria-label={t(($) => $.orchestration.loading)}><Skeleton className="h-24 w-full rounded-lg" /></div>;
  }
  if (query.isError || !summary) {
    return (
      <div className="border-b p-4">
        <div role="alert" className="flex flex-wrap items-center gap-3 rounded-lg border border-destructive/30 bg-destructive/5 p-4">
          <AlertCircle className="size-5 text-destructive" />
          <p className="min-w-0 flex-1 text-sm">{t(($) => $.orchestration.error)}</p>
          <Button size="sm" variant="outline" onClick={() => void query.refetch()} disabled={query.isFetching}>
            <RefreshCw className={cn("size-4", query.isFetching && "animate-spin motion-reduce:animate-none")} />
            {t(($) => $.orchestration.retry)}
          </Button>
        </div>
      </div>
    );
  }

  const active = summary.issues.reduce((total, issue) => total + issue.active_tasks, 0);
  const ready = summary.issues.reduce((total, issue) => total + issue.ready_tasks, 0);
  return (
    <section className="border-b p-4" aria-labelledby="project-orchestration-heading">
      <div className={cn("rounded-xl border p-4", tone[summary.classification])}>
        <div className="flex flex-wrap items-start gap-3">
          <div className="min-w-0 flex-1">
            <div className="flex items-center gap-2">
              <h2 id="project-orchestration-heading" className="text-sm font-semibold">{t(($) => $.orchestration.title)}</h2>
              <span className="rounded-full border bg-background/70 px-2 py-0.5 text-xs font-medium">
                {t(($) => $.orchestration.classification[summary.classification])}
              </span>
            </div>
            <p className="mt-1 text-sm text-muted-foreground">{summary.reason.message}</p>
          </div>
          <Button size="sm" variant="outline" title={t(($) => $.orchestration.refresh_hint)} onClick={() => void query.refetch()} disabled={query.isFetching}>
            <RefreshCw className={cn("size-4", query.isFetching && "animate-spin motion-reduce:animate-none")} />
            {t(($) => $.orchestration.refresh)}
          </Button>
        </div>
        <dl className="mt-4 grid grid-cols-2 gap-3 sm:max-w-sm">
          <div><dt className="text-xs text-muted-foreground">{t(($) => $.orchestration.active)}</dt><dd className="text-lg font-semibold tabular-nums">{active}</dd></div>
          <div><dt className="text-xs text-muted-foreground">{t(($) => $.orchestration.ready)}</dt><dd className="text-lg font-semibold tabular-nums">{ready}</dd></div>
        </dl>
        {summary.issues.length === 0 ? (
          <p className="mt-4 text-sm text-muted-foreground">{t(($) => $.orchestration.empty)}</p>
        ) : (
          <ul className="mt-4 divide-y rounded-lg border bg-background/70" aria-label={t(($) => $.orchestration.issue_states)}>
            {summary.issues.map((issue) => (
              <li key={issue.issue_id} className="flex items-start gap-3 px-3 py-2.5">
                <StateIcon state={issue.execution_state} />
                <div className="min-w-0 flex-1"><p className="truncate text-sm font-medium">{issue.issue_id}</p><p className="text-xs text-muted-foreground">{issue.reason.message}</p></div>
                <span className="shrink-0 text-xs tabular-nums text-muted-foreground">{issue.active_tasks} / {issue.ready_tasks}</span>
              </li>
            ))}
          </ul>
        )}
        {summary.self_iteration_candidates.length > 0 && (
          <div className="mt-4">
            <h3 className="flex items-center gap-2 text-sm font-medium"><Sparkles className="size-4" />{t(($) => $.orchestration.candidates)}</h3>
            <ul className="mt-2 space-y-2">
              {summary.self_iteration_candidates.map((candidate) => <li key={candidate.id} className="rounded-lg border bg-background/70 px-3 py-2"><p className="text-sm font-medium">{candidate.title}</p><p className="text-xs text-muted-foreground">{candidate.reason}</p></li>)}
            </ul>
          </div>
        )}
      </div>
    </section>
  );
}
