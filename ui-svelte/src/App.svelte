<script lang="ts">
  import { onMount } from "svelte";
  import type { ComponentType } from "svelte";
  import Router from "svelte-spa-router";
  import wrap from "svelte-spa-router/wrap";
  import Header from "./components/Header.svelte";
  import { enableAPIEvents } from "./stores/api";
  import { initScreenWidth, isDarkMode, appTitle, connectionState } from "./stores/theme";
  import { currentRoute } from "./stores/route";

  function lazyRoute(loader: () => Promise<{ default: unknown }>) {
    return wrap({
      asyncComponent: async () => {
        const mod = await loader();
        return { default: mod.default as ComponentType };
      },
    });
  }

  const routes = {
    "/": lazyRoute(() => import("./routes/Playground.svelte")),
    "/models": lazyRoute(() => import("./routes/Models.svelte")),
    "/hf-models": lazyRoute(() => import("./routes/HFModels.svelte")),
    "/images": lazyRoute(() => import("./routes/Images.svelte")),
    "/logs": lazyRoute(() => import("./routes/LogViewer.svelte")),
    "/cluster": lazyRoute(() => import("./routes/ClusterStatus.svelte")),
    "/editor": lazyRoute(() => import("./routes/ConfigEditor.svelte")),
    "/credit": lazyRoute(() => import("./routes/Help.svelte")),
    "/activity": lazyRoute(() => import("./routes/Activity.svelte")),
    "*": lazyRoute(() => import("./routes/Playground.svelte")),
  };

  function handleRouteLoaded(event: { detail: { route: string | RegExp } }) {
    const route = event.detail.route;
    currentRoute.set(typeof route === "string" ? route : "/");
  }

  $effect(() => {
    document.documentElement.setAttribute("data-theme", $isDarkMode ? "dark" : "light");
  });

  $effect(() => {
    const icon = $connectionState === "connecting" ? "\u{1F7E1}" : $connectionState === "connected" ? "\u{1F7E2}" : "\u{1F534}";
    document.title = `${icon} ${$appTitle}`;
  });

  onMount(() => {
    const cleanupScreenWidth = initScreenWidth();
    enableAPIEvents(true);

    return () => {
      cleanupScreenWidth();
      enableAPIEvents(false);
    };
  });
</script>

<div class="flex flex-col h-screen">
  <Header />

  <main class="flex-1 overflow-auto p-4">
    <div class="h-full">
      <Router {routes} on:routeLoaded={handleRouteLoaded} />
    </div>
  </main>
</div>
