import { Platform } from "react-native";
import { resolveClientOS } from "./resolve-client-os";

/** Platform identity sent to the API and realtime server. */
export function getClientOS(platformOS: string = Platform.OS): string {
  return resolveClientOS(platformOS);
}
