const { contextBridge, ipcRenderer } = require('electron');

contextBridge.exposeInMainWorld('api', {
  openExternal: (url) => {
    ipcRenderer.send('open-external', url);
  },
  hideToTray: () => {
    ipcRenderer.send('hide-to-tray');
  },
  getConfig: () => ipcRenderer.invoke('get-config'),
  saveConfig: (config) => ipcRenderer.invoke('save-config', config)
});
