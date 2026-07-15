<script lang="ts">
  export let name:
    | 'server'
    | 'events'
    | 'settings'
    | 'plus'
    | 'terminal'
    | 'play'
    | 'stop'
    | 'restart'
    | 'trash'
    | 'close'
    | 'alert'
    | 'check'
    | 'info'
    | 'chevron'
    | 'refresh'
    | 'shield'
    | 'database' = 'server';
  export let size = 18;
  export let strokeWidth = 1.7;
</script>

<svg
  width={size}
  height={size}
  viewBox="0 0 24 24"
  fill="none"
  stroke="currentColor"
  stroke-width={strokeWidth}
  stroke-linecap="square"
  stroke-linejoin="miter"
  aria-hidden="true"
  focusable="false"
>
  {#if name === 'server'}
    <rect x="3" y="4" width="18" height="6" rx="1" />
    <rect x="3" y="14" width="18" height="6" rx="1" />
    <path d="M7 7h.01M7 17h.01M11 7h7M11 17h7" />
  {:else if name === 'events'}
    <path d="M5 5h14M5 12h14M5 19h14" />
    <path d="M3 5h.01M3 12h.01M3 19h.01" />
  {:else if name === 'settings'}
    <circle cx="12" cy="12" r="3" />
    <path d="M12 2v3M12 19v3M4.93 4.93l2.12 2.12M16.95 16.95l2.12 2.12M2 12h3M19 12h3M4.93 19.07l2.12-2.12M16.95 7.05l2.12-2.12" />
  {:else if name === 'plus'}
    <path d="M12 5v14M5 12h14" />
  {:else if name === 'terminal'}
    <rect x="3" y="4" width="18" height="16" rx="1" />
    <path d="m7 9 3 3-3 3M13 15h4" />
  {:else if name === 'play'}
    <path d="m8 5 11 7-11 7V5Z" />
  {:else if name === 'stop'}
    <rect x="6" y="6" width="12" height="12" />
  {:else if name === 'restart'}
    <path d="M20 11a8 8 0 1 0-2.34 5.66M20 5v6h-6" />
  {:else if name === 'trash'}
    <path d="M4 7h16M9 7V4h6v3M7 7l1 13h8l1-13M10 11v5M14 11v5" />
  {:else if name === 'close'}
    <path d="M6 6l12 12M18 6 6 18" />
  {:else if name === 'alert'}
    <path d="M12 3 2.5 20h19L12 3Z" />
    <path d="M12 9v5M12 17h.01" />
  {:else if name === 'check'}
    <path d="m5 12 4 4L19 6" />
  {:else if name === 'info'}
    <circle cx="12" cy="12" r="9" />
    <path d="M12 11v6M12 7h.01" />
  {:else if name === 'chevron'}
    <path d="m9 6 6 6-6 6" />
  {:else if name === 'refresh'}
    <path d="M20 11a8 8 0 1 0-2.34 5.66M20 5v6h-6" />
  {:else if name === 'shield'}
    <path d="M12 3 5 6v5c0 4.8 2.8 8 7 10 4.2-2 7-5.2 7-10V6l-7-3Z" />
    <path d="m9 12 2 2 4-5" />
  {:else if name === 'database'}
    <ellipse cx="12" cy="5" rx="8" ry="3" />
    <path d="M4 5v7c0 1.7 3.6 3 8 3s8-1.3 8-3V5M4 12v7c0 1.7 3.6 3 8 3s8-1.3 8-3v-7" />
  {/if}
</svg>
