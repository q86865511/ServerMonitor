import './style.css'
import { mount } from 'svelte'
import App from './App.svelte'

// App.svelte 為 runes 元件(R2 shell 重寫起改用 $state),Svelte 5 runes 元件不再支援
// `new Component(...)` 舊建構子,需以 mount() 掛載。
const app = mount(App, {
  target: document.getElementById('app')!
})

export default app
