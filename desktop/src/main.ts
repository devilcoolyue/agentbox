import { createApp } from 'vue';
import App from './App.vue';
import './style.css';

const app=createApp(App);
if (import.meta.env.VITE_AGENTBOX_SMOKE === '1') {
 app.config.errorHandler=(error)=>{void import('./smoke').then(module=>module.reportSmokeError(error));};
 window.addEventListener('error',event=>{void import('./smoke').then(module=>module.reportSmokeError(event.error||event.message));});
 window.addEventListener('unhandledrejection',event=>{void import('./smoke').then(module=>module.reportSmokeError(event.reason));});
}
app.mount('#app');
if (import.meta.env.VITE_AGENTBOX_SMOKE === '1') void import('./smoke').then(module => module.runSmoke());
