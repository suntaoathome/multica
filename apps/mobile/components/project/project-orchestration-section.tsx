import { Alert, Pressable, View } from "react-native";
import { Ionicons } from "@expo/vector-icons";
import { useQuery } from "@tanstack/react-query";
import type { IssueOrchestrationSummary } from "@multica/core/types";
import { Text } from "@/components/ui/text";
import { Button } from "@/components/ui/button";
import { projectOrchestrationOptions } from "@/data/queries/projects";
import { useRecoverProjectOrchestration } from "@/data/mutations/projects";

const CLASSIFICATION_LABELS = {
  running: "Running",
  ready: "Ready",
  complete: "Complete",
  waiting_external: "Waiting externally",
  temporarily_not_ready: "Temporarily not ready",
  orchestration_fault: "Orchestration fault",
} as const;

const STATE_ICONS: Record<IssueOrchestrationSummary["execution_state"], keyof typeof Ionicons.glyphMap> = {
  running: "sync",
  ready: "play-circle-outline",
  waiting: "time-outline",
  temporarily_not_ready: "pause-circle-outline",
  faulted: "alert-circle-outline",
  complete: "checkmark-circle-outline",
};

function eventLabel(event: IssueOrchestrationSummary["last_event"]) {
  if (!event) return "No event recorded";
  const date = new Date(event.created_at);
  const time = Number.isNaN(date.getTime()) ? event.created_at : date.toLocaleString();
  return `${event.type} · ${time}`;
}

export function ProjectOrchestrationSection({
  projectId,
  wsId,
  isWorkspaceAdmin,
}: {
  projectId: string;
  wsId: string | null;
  isWorkspaceAdmin: boolean;
}) {
  const query = useQuery(projectOrchestrationOptions(wsId, projectId));
  const recovery = useRecoverProjectOrchestration(projectId);
  const summary = query.data;

  const recover = (issue: IssueOrchestrationSummary) => {
    const action = issue.recovery_action;
    if (!action || !isWorkspaceAdmin || action.allowed !== true) return;
    Alert.alert("Resume this issue?", action.side_effect, [
      { text: "Cancel", style: "cancel" },
      {
        text: "Resume",
        onPress: () => recovery.mutate(issue.issue_id, {
          onSuccess: (result) => Alert.alert(
            result.applied ? "Recovery started" : "Already active",
            result.applied
              ? "A new assignment run was queued."
              : "An active execution already exists; no duplicate run was created.",
          ),
          onError: () => Alert.alert("Recovery failed", "Try again after refreshing the project."),
        }),
      },
    ]);
  };

  return (
    <View className="border-b border-border px-4 py-5 gap-3">
      <View className="flex-row items-center justify-between gap-3">
        <Text className="text-base font-semibold">Orchestration</Text>
        <Pressable
          onPress={() => query.refetch()}
          disabled={query.isFetching}
          accessibilityRole="button"
          accessibilityLabel="Refresh orchestration status"
          className="p-2"
        >
          <Ionicons name="refresh" size={18} className="text-muted-foreground" />
        </Pressable>
      </View>

      {query.isLoading ? (
        <Text className="text-sm text-muted-foreground">Loading orchestration status…</Text>
      ) : query.isError || !summary || summary.project_id === "" ? (
        <View className="gap-2">
          <Text className="text-sm text-destructive">Orchestration status is unavailable.</Text>
          <Button variant="outline" onPress={() => query.refetch()}><Text>Retry</Text></Button>
        </View>
      ) : (
        <View className="gap-3 rounded-xl border border-border bg-muted/30 p-4">
          <View className="gap-1">
            <Text className="text-sm font-semibold">{CLASSIFICATION_LABELS[summary.classification]}</Text>
            <Text className="text-sm text-muted-foreground">{summary.reason.message}</Text>
            <Text className="text-xs text-muted-foreground">{eventLabel(summary.last_event)}</Text>
          </View>
          <View className="flex-row gap-8">
            <View><Text className="text-xs text-muted-foreground">Running slots</Text><Text className="text-lg font-semibold">{summary.running_slots} / {summary.capacity}</Text></View>
            <View><Text className="text-xs text-muted-foreground">Ready</Text><Text className="text-lg font-semibold">{summary.issues.reduce((total, issue) => total + issue.ready_tasks, 0)}</Text></View>
          </View>

          {summary.issues.map((issue) => {
            const action = issue.recovery_action;
            const allowed = isWorkspaceAdmin && action?.allowed === true;
            const disabledReason = !isWorkspaceAdmin ? "Workspace admin access required" : action?.reason;
            return (
              <View key={issue.issue_id} className="border-t border-border pt-3 gap-2">
                <View className="flex-row items-start gap-2">
                  <Ionicons name={STATE_ICONS[issue.execution_state]} size={18} className="text-muted-foreground" />
                  <View className="flex-1 gap-1">
                    <Text className="text-sm font-medium" numberOfLines={1}>{issue.issue_id}</Text>
                    <Text className="text-xs text-muted-foreground">{issue.reason.message}</Text>
                    <Text className="text-xs text-muted-foreground">{eventLabel(issue.last_event)}</Text>
                    <Text className="text-xs text-muted-foreground">Active / ready {issue.active_tasks} / {issue.ready_tasks} · Slots {issue.running_slots} / {issue.capacity}</Text>
                  </View>
                </View>
                {action ? (
                  <View className="items-start gap-1">
                    <Button variant="outline" disabled={!allowed || recovery.isPending} onPress={() => recover(issue)}><Text>{recovery.isPending && recovery.variables === issue.issue_id ? "Recovering…" : "Recover"}</Text></Button>
                    {!allowed ? <Text className="text-xs text-muted-foreground">{disabledReason}</Text> : null}
                  </View>
                ) : null}
              </View>
            );
          })}

          {summary.self_iteration_candidates.length > 0 ? (
            <View className="border-t border-border pt-3 gap-2">
              <Text className="text-sm font-semibold">Iteration candidates</Text>
              {summary.self_iteration_candidates.map((candidate) => (
                <View key={candidate.id} className="rounded-lg border border-border bg-background p-3 gap-1">
                  <Text className="text-sm font-medium">{candidate.title}</Text>
                  <Text className="text-xs text-muted-foreground">{candidate.reason}</Text>
                  <Text className="text-xs text-muted-foreground">{candidate.state}</Text>
                </View>
              ))}
            </View>
          ) : null}
        </View>
      )}
    </View>
  );
}
