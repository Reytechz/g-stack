const { app, BrowserWindow, Tray, Menu, nativeImage, shell, ipcMain, dialog } = require('electron');
const { autoUpdater } = require('electron-updater');
const { spawn } = require('child_process');
const path = require('path');
const fs = require('fs');

// Set application name and desktop name for Linux window manager mapping
app.setName('G-Stack');

let mainWindow;
let tray;
let gstackProcess;
let isQuitting = false;

// Get data directory: project root in dev, appData folder in prod
const dataDir = app.isPackaged ? app.getPath('userData') : __dirname;
const configPath = path.join(dataDir, 'config.json');

// Ensure writable config exists in prod by copying from package if missing
function ensureConfigExists() {
  if (app.isPackaged && !fs.existsSync(configPath)) {
    try {
      const defaultPackagedConfig = path.join(__dirname, 'config.json');
      if (fs.existsSync(defaultPackagedConfig)) {
        fs.mkdirSync(dataDir, { recursive: true });
        fs.copyFileSync(defaultPackagedConfig, configPath);
        console.log(`[App] Copied default config to user data directory: ${configPath}`);
      }
    } catch (err) {
      console.error('Failed to copy default config:', err);
    }
  }
}

// Base64 green cloud icon (16x16) for system tray
const iconBase64 = 'iVBORw0KGgoAAAANSUhEUgAAABAAAAAQCAYAAAAf8/9hAAAAm0lEQVQ4T2NkoBAwUqifAWJgRFN8H4j/M2BTI8rA+B8hG59afA5gYGBg+M8w9T8DAwMDiAYpBtVgYAQSDAwMDL9hCrCpwWYALq/A/AuyApsBqLwCDUDlFWgAKq9AA1B5BRqAyivQAFS1oQGoeh/oAKqeR2D+x1C5e+NzAAMDw3+Y/A8kGlENM2AyABv4h2oIMh2i7Bco2gBqjR4wE24dRwAAAABJRU5ErkJggg==';

function writeIconFile(filePath) {
  try {
    const buffer = Buffer.from(iconBase64, 'base64');
    fs.writeFileSync(filePath, buffer);
  } catch (err) {
    console.error('Failed to write icon file:', err);
  }
}

function startBackend() {
  const binaryName = process.platform === 'win32' ? 'gstack.exe' : 'gstack';
  let binaryPath;

  if (app.isPackaged) {
    binaryPath = path.join(process.resourcesPath, binaryName);
  } else {
    binaryPath = path.join(__dirname, process.platform === 'win32' ? 'gstack.exe' : './gstack');
  }

  // Set executable permissions on non-Windows platforms in production
  if (process.platform !== 'win32') {
    try {
      if (fs.existsSync(binaryPath)) {
        fs.chmodSync(binaryPath, '755');
      }
    } catch (err) {
      console.error('Failed to set executable permissions on daemon:', err);
    }
  }

  console.log(`Starting backend daemon: ${binaryPath} in working dir: ${dataDir}`);
  
  gstackProcess = spawn(binaryPath, [], {
    cwd: dataDir,
    env: { ...process.env }
  });

  gstackProcess.stdout.on('data', (data) => {
    console.log(`[Go Engine]: ${data.toString().trim()}`);
  });

  gstackProcess.stderr.on('data', (data) => {
    console.error(`[Go Engine Err]: ${data.toString().trim()}`);
  });

  gstackProcess.on('close', (code) => {
    console.log(`Backend daemon exited with code ${code}`);
    if (!isQuitting) {
      console.log('Daemon exited unexpectedly. Restarting in 3 seconds...');
      setTimeout(startBackend, 3000);
    }
  });
}

function createWindow() {
  const iconPath = path.join(__dirname, 'icon.png');
  mainWindow = new BrowserWindow({
    width: 820,
    height: 650,
    resizable: false,
    frame: true, // standard window
    webPreferences: {
      nodeIntegration: false,
      contextIsolation: true,
      preload: path.join(__dirname, 'preload.js')
    },
    icon: nativeImage.createFromPath(iconPath),
    backgroundColor: '#0d0e12',
    show: false
  });

  mainWindow.loadFile('index.html');
  mainWindow.setMenuBarVisibility(false); // Hide standard menu bar

  // Prevent flash during load
  mainWindow.once('ready-to-show', () => {
    mainWindow.show();
  });

  // Intercept links and open in external system browser
  mainWindow.webContents.setWindowOpenHandler(({ url }) => {
    openExternalUrl(url);
    return { action: 'deny' };
  });

  // Minimize to tray on close
  mainWindow.on('close', (event) => {
    if (!isQuitting) {
      event.preventDefault();
      mainWindow.hide();
    }
    return false;
  });
}

function setupAutoUpdater() {
  // Check for updates on startup
  console.log('[Updater] Checking for updates...');
  autoUpdater.checkForUpdatesAndNotify();

  // Check for updates every 2 hours
  setInterval(() => {
    autoUpdater.checkForUpdatesAndNotify();
  }, 2 * 60 * 60 * 1000);

  autoUpdater.on('update-available', () => {
    console.log('[Updater] Update available.');
  });

  autoUpdater.on('update-downloaded', (info) => {
    console.log('[Updater] Update downloaded:', info.version);
    dialog.showMessageBox(mainWindow, {
      type: 'info',
      title: 'Update Available',
      message: `A new version (${info.version}) of G-Stack has been downloaded.`,
      detail: 'Would you like to restart the application to apply the update now?',
      buttons: ['Restart and Install', 'Later'],
      defaultId: 0,
      cancelId: 1
    }).then((result) => {
      if (result.response === 0) {
        isQuitting = true;
        autoUpdater.quitAndInstall();
      }
    });
  });

  autoUpdater.on('error', (err) => {
    console.error('[Updater] Error checking for updates:', err);
  });
}

function createTray() {
  const iconPath = path.join(__dirname, 'icon.png');
  if (!fs.existsSync(iconPath)) {
    writeIconFile(iconPath);
  }

  const image = nativeImage.createFromPath(iconPath);
  tray = new Tray(image);
  tray.setToolTip('G-Stack');

  const contextMenu = Menu.buildFromTemplate([
    {
      label: 'Show',
      click: () => {
        mainWindow.show();
      }
    },
    {
      label: 'Mount & Open Drive',
      click: () => {
        let username = 'admin';
        let addr = 'localhost:8080';
        let password = 'admin';
        try {
          if (fs.existsSync(configPath)) {
            const config = JSON.parse(fs.readFileSync(configPath, 'utf8'));
            username = config.username || username;
            addr = config.addr || addr;
            password = config.password || password;
          }
        } catch (err) {
          console.error('Failed to read config for tray mount:', err);
        }
        const mountUrl = `dav://${username}:${password}@${addr}/G-Stack/`;
        openExternalUrl(mountUrl);
      }
    },
    { type: 'separator' },
    {
      label: 'Quit Application',
      click: () => {
        isQuitting = true;
        app.quit();
      }
    }
  ]);

  tray.setContextMenu(contextMenu);

  tray.on('double-click', () => {
    mainWindow.show();
  });
}

// Single instance lock
const gotTheLock = app.requestSingleInstanceLock();
if (!gotTheLock) {
  app.quit();
} else {
  app.on('second-instance', () => {
    if (mainWindow) {
      if (mainWindow.isMinimized()) mainWindow.restore();
      mainWindow.show();
      mainWindow.focus();
    }
  });

  app.whenReady().then(() => {
    ensureConfigExists();

    const iconPath = path.join(__dirname, 'icon.png');
    if (!fs.existsSync(iconPath)) {
      writeIconFile(iconPath);
    }

    Menu.setApplicationMenu(null); // Remove default application menu bar
    startBackend();
    createWindow();
    createTray();
    setupAutoUpdater();

    app.on('activate', () => {
      if (BrowserWindow.getAllWindows().length === 0) {
        createWindow();
      }
    });
  });
}

app.on('will-quit', () => {
  isQuitting = true;
  if (gstackProcess) {
    console.log('Terminating backend daemon...');
    gstackProcess.kill();
  }
});

// Listen for custom close/hide button from frontend
ipcMain.on('hide-to-tray', () => {
  if (mainWindow) {
    mainWindow.hide();
  }
});

// Helper to show clipboard copy dialog if browser fails to open
function showCopyDialog(url) {
  const { clipboard } = require('electron');
  dialog.showMessageBox(mainWindow, {
    type: 'info',
    title: 'Authorization Link',
    message: 'Could not launch the system web browser automatically.',
    detail: `Please copy the authorization link below and paste it into your browser:\n\n${url}`,
    buttons: ['Copy Link', 'Close'],
    defaultId: 0
  }).then(({ response }) => {
    if (response === 0) {
      clipboard.writeText(url);
      console.log(`[IPC] URL copied to clipboard: ${url}`);
    }
  });
}

// Helper to show manual mount instructions dialog
function showMountDialog(url) {
  let username = 'admin';
  let addr = 'localhost:8080';
  let password = 'admin';
  try {
    if (fs.existsSync(configPath)) {
      const config = JSON.parse(fs.readFileSync(configPath, 'utf8'));
      username = config.username || username;
      addr = config.addr || addr;
      password = config.password || password;
    }
  } catch (err) {
    console.error('Failed to read config for dialog:', err);
  }

  dialog.showMessageBox(mainWindow, {
    type: 'info',
    title: 'Manual Mount Instructions',
    message: 'How to mount G-Stack in your File Manager:',
    detail: `If your file manager did not open automatically, you can mount G-Stack manually:\n\n` +
            `1. Open Nautilus (Files) or Dolphin.\n` +
            `2. Press Ctrl+L and type: dav://${username}@${addr}/\n` +
            `3. Press Enter and connect using Password: ${password}\n\n` +
            `Alternatively, you can run this command in your terminal:\n` +
            `gio mount dav://${username}@${addr}/`,
    buttons: ['Copy Command', 'Close'],
    defaultId: 0
  }).then(({ response }) => {
    if (response === 0) {
      const { clipboard } = require('electron');
      clipboard.writeText(`gio mount dav://${username}@${addr}/`);
      console.log(`[App] gio mount command copied to clipboard`);
    }
  });
}

// Reusable function to open external URLs with system-native fallbacks
function openExternalUrl(url) {
  console.log(`[App] openExternalUrl requested for: ${url}`);
  
  if (url.startsWith('dav://') || url.startsWith('davs://')) {
    if (process.platform === 'linux') {
      const { exec } = require('child_process');
      console.log(`[App] Linux WebDAV mount requested: ${url}`);
      
      // Try Dolphin first since the user prefers it
      const dolphinUrl = url.replace('dav://', 'webdav://').replace('davs://', 'webdavs://');
      exec(`dolphin "${dolphinUrl}"`, (dolphinErr) => {
        if (dolphinErr) {
          console.warn(`[App] Dolphin mount failed:`, dolphinErr);
          
          // Try Nautilus
          exec(`nautilus "${url}"`, (nautilusErr) => {
            if (nautilusErr) {
              console.warn(`[App] Nautilus mount failed:`, nautilusErr);
              
              // Try registering mount using gio
              exec(`gio mount "${url}"`, (gioErr) => {
                if (gioErr) {
                  console.error(`[App] GIO mount failed:`, gioErr);
                }
                showMountDialog(url);
              });
            } else {
              console.log(`[App] WebDAV successfully opened in Nautilus`);
            }
          });
        } else {
          console.log(`[App] WebDAV successfully opened in Dolphin`);
        }
      });
      return;
    }
  }

  shell.openExternal(url).then(() => {
    console.log(`[App] shell.openExternal successfully launched URL`);
  }).catch((err) => {
    console.error(`[App] shell.openExternal failed for ${url}:`, err);
    
    // Fallback: try spawning xdg-open on Linux
    if (process.platform === 'linux') {
      const { exec } = require('child_process');
      console.log(`[App] Attempting xdg-open fallback...`);
      exec(`xdg-open "${url.replace(/"/g, '\\"')}"`, (execErr) => {
        if (execErr) {
          console.error(`[App] xdg-open fallback failed:`, execErr);
          
          // Try firefox fallback directly
          console.log(`[App] Attempting firefox fallback...`);
          exec(`firefox "${url.replace(/"/g, '\\"')}"`, (ffErr) => {
            if (ffErr) {
              console.error(`[App] Firefox fallback failed:`, ffErr);
              showCopyDialog(url);
            } else {
              console.log(`[App] Firefox fallback successfully launched browser`);
            }
          });
        } else {
          console.log(`[App] xdg-open fallback successfully launched browser`);
        }
      });
    } else {
      showCopyDialog(url);
    }
  });
}

// Listen for external link requests from frontend
ipcMain.on('open-external', (event, url) => {
  openExternalUrl(url);
});

// Restart the backend daemon gracefully
function restartBackend() {
  if (gstackProcess) {
    console.log('[App] Force restarting backend daemon...');
    gstackProcess.removeAllListeners('close');
    gstackProcess.kill();
  }
  startBackend();
}

// IPC handler to fetch config
ipcMain.handle('get-config', async () => {
  try {
    if (fs.existsSync(configPath)) {
      const content = fs.readFileSync(configPath, 'utf8');
      return JSON.parse(content);
    }
  } catch (err) {
    console.error('Failed to read config:', err);
  }
  return {
    username: 'admin',
    password: 'admin',
    addr: 'localhost:8080'
  };
});

// IPC handler to save config and restart Go daemon
ipcMain.handle('save-config', async (event, newConfig) => {
  try {
    let existingConfig = {};
    if (fs.existsSync(configPath)) {
      existingConfig = JSON.parse(fs.readFileSync(configPath, 'utf8'));
    }
    
    const updated = {
      ...existingConfig,
      username: newConfig.username,
      password: newConfig.password,
      addr: newConfig.addr,
      client_id: newConfig.client_id || existingConfig.client_id,
      client_secret: newConfig.client_secret || existingConfig.client_secret
    };
    
    fs.writeFileSync(configPath, JSON.stringify(updated, null, 2), 'utf8');
    console.log('[App] Saved new config to config.json');
    
    restartBackend();
    return { success: true };
  } catch (err) {
    console.error('Failed to save config:', err);
    return { success: false, error: err.message };
  }
});



