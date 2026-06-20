let appConfig = {
  addr: 'localhost:8080',
  username: 'admin',
  password: 'admin'
};

const POLLING_INTERVAL = 4000; // 4 seconds

const poolProgressRing = document.getElementById('pool-progress-ring');
const poolPercentageText = document.getElementById('pool-percentage');
const poolCapacityText = document.getElementById('pool-capacity');
const poolUsedText = document.getElementById('pool-used');
const poolFreeText = document.getElementById('pool-free');
const accountsCountText = document.getElementById('accounts-count');
const accountsListContainer = document.getElementById('accounts-list-container');
const btnMountDrive = document.getElementById('btn-mount-drive');
const btnCopyWebdav = document.getElementById('btn-copy-webdav');
const webdavUrlText = document.getElementById('webdav-url');
const daemonStatusText = document.getElementById('daemon-status-text');
const refreshTimeText = document.getElementById('refresh-time');
const refreshSpinner = document.getElementById('refresh-spinner');

// Settings Elements
const settingsModal = document.getElementById('settings-modal');
const btnSettings = document.getElementById('btn-settings');
const btnCloseSettings = document.getElementById('btn-close-settings');
const btnCancelSettings = document.getElementById('btn-cancel-settings');
const btnSaveSettings = document.getElementById('btn-save-settings');

// Calculate circle circumference
const radius = 85;
const circumference = 2 * Math.PI * radius;
poolProgressRing.style.strokeDasharray = circumference;
poolProgressRing.style.strokeDashoffset = circumference;

// Format bytes to human readable format
function formatBytes(bytes) {
  if (bytes === 0) return '0 GB';
  const k = 1024;
  const sizes = ['Bytes', 'KB', 'MB', 'GB', 'TB'];
  const i = Math.floor(Math.log(bytes) / Math.log(k));
  
  if (i < 3) {
    return (bytes / (k * k * k)).toFixed(2) + ' GB';
  }
  return parseFloat((bytes / Math.pow(k, i)).toFixed(2)) + ' ' + sizes[i];
}

// Set SVG progress ring offset
function setProgress(percent) {
  const offset = circumference - (percent / 100) * circumference;
  poolProgressRing.style.strokeDashoffset = offset;
}

let isPasswordVisible = false;

function updateCredsDisplay() {
  const usernameEl = document.getElementById('creds-username');
  const passwordEl = document.getElementById('creds-password');
  if (usernameEl) usernameEl.textContent = appConfig.username || 'admin';
  if (passwordEl) {
    passwordEl.textContent = isPasswordVisible ? (appConfig.password || 'admin') : '••••••••';
  }
}

// Fetch active config from Electron IPC
async function loadConfig() {
  if (window.api && window.api.getConfig) {
    try {
      const config = await window.api.getConfig();
      if (config) {
        appConfig = config;
        
        // Update mounting address UI
        webdavUrlText.textContent = `http://${appConfig.addr}/G-Stack/`;
        updateCredsDisplay();
        
        // Fill settings form inputs
        document.getElementById('input-addr').value = appConfig.addr || 'localhost:8080';
        document.getElementById('input-username').value = appConfig.username || 'admin';
        document.getElementById('input-password').value = appConfig.password || 'admin';
        document.getElementById('input-client-id').value = appConfig.client_id || '';
        document.getElementById('input-client-secret').value = appConfig.client_secret || '';
      }
    } catch (err) {
      console.error('Failed to load configuration from main process:', err);
    }
  }
}

// Fetch status from Go API using configured address
async function fetchStatus() {
  refreshSpinner.style.display = 'inline-block';
  
  try {
    const response = await fetch(`http://${appConfig.addr}/status`);
    if (!response.ok) throw new Error('API server returned error status');
    
    const data = await response.json();
    
    // Update daemon indicator
    daemonStatusText.textContent = 'Daemon Active';
    daemonStatusText.parentElement.style.background = 'rgba(0, 230, 118, 0.08)';
    daemonStatusText.parentElement.style.color = 'var(--color-green)';
    daemonStatusText.parentElement.querySelector('.status-indicator').style.backgroundColor = 'var(--color-green)';
    daemonStatusText.parentElement.querySelector('.status-indicator').style.boxShadow = '0 0 10px var(--color-green)';
    
    // Update aggregate metrics
    const capacity = data.total_capacity || 0;
    const used = data.total_used_space || 0;
    const free = data.free_space || 0;
    const percentage = capacity > 0 ? Math.round((used / capacity) * 100) : 0;
    
    poolPercentageText.textContent = `${percentage}%`;
    setProgress(percentage);
    
    poolCapacityText.textContent = formatBytes(capacity);
    poolUsedText.textContent = formatBytes(used);
    poolFreeText.textContent = formatBytes(free);
    
    // Update connected accounts count
    accountsCountText.textContent = `${data.accounts_count} Connected`;
    
    // Update accounts list
    renderAccounts(data.accounts || []);
    
    // Update active background uploads progress
    await fetchActiveUploads();
    
    const now = new Date();
    refreshTimeText.textContent = `Synced at ${now.toLocaleTimeString()}`;
    
  } catch (error) {
    console.error('Failed to fetch status:', error);
    
    // Show offline status on active uploads
    const uploadsListContainer = document.getElementById('uploads-list-container');
    const uploadsCountText = document.getElementById('uploads-count');
    if (uploadsListContainer && uploadsCountText) {
      uploadsCountText.textContent = 'Offline';
      uploadsListContainer.innerHTML = `
        <div class="empty-state">
          <p style="color: var(--color-red)">Connection offline</p>
          <p class="sub-empty">Unable to retrieve uploads status from daemon.</p>
        </div>
      `;
    }

    // Show offline status unless we are currently saving/rebooting
    if (btnSaveSettings && btnSaveSettings.disabled) {
      daemonStatusText.textContent = 'Rebooting Daemon...';
    } else {
      daemonStatusText.textContent = 'Daemon Offline';
      daemonStatusText.parentElement.style.background = 'rgba(255, 61, 0, 0.08)';
      daemonStatusText.parentElement.style.color = 'var(--color-red)';
      daemonStatusText.parentElement.querySelector('.status-indicator').style.backgroundColor = 'var(--color-red)';
      daemonStatusText.parentElement.querySelector('.status-indicator').style.boxShadow = '0 0 10px var(--color-red)';
      
      poolPercentageText.textContent = '0%';
      setProgress(0);
      poolCapacityText.textContent = '0 GB';
      poolUsedText.textContent = '0 GB';
      poolFreeText.textContent = '0 GB';
      accountsCountText.textContent = '0 Connected';
      refreshTimeText.textContent = 'Failed to sync';
      
      // Render offline empty state
      accountsListContainer.innerHTML = `
        <div class="empty-state">
          <p style="color: var(--color-red)">Cannot connect to Go daemon.</p>
          <p class="sub-empty">Please verify G-Stack background service is running on ${appConfig.addr}.</p>
        </div>
      `;
    }
  } finally {
    refreshSpinner.style.display = 'none';
  }
}

// Render connected accounts in the list
function renderAccounts(accounts) {
  accountsListContainer.innerHTML = '';
  
  if (accounts.length === 0) {
    const emptyState = document.createElement('div');
    emptyState.className = 'empty-state';
    emptyState.innerHTML = `
      <p>No Google Drive accounts connected yet.</p>
      <p class="sub-empty">Connect at least one account to start pooling storage space.</p>
    `;
    accountsListContainer.appendChild(emptyState);
  } else {
    accounts.forEach(acc => {
      const usagePercent = acc.capacity > 0 ? Math.round((acc.used_space / acc.capacity) * 100) : 0;
      
      const item = document.createElement('div');
      item.className = 'account-item';
      item.innerHTML = `
        <div class="acc-info">
          <span class="acc-email">${acc.email}</span>
          <span class="acc-storage-text">${formatBytes(acc.used_space)} / ${formatBytes(acc.capacity)} used</span>
        </div>
        <div class="acc-progress-container">
          <div class="progress-bar-bg">
            <div class="progress-bar-fill" style="width: ${usagePercent}%"></div>
          </div>
          <button class="btn-remove" title="Unlink account" data-email="${acc.email}">
            <svg width="16" height="16" fill="none" stroke="currentColor" stroke-width="2" viewBox="0 0 24 24"><path d="M19 7l-.867 12.142A2 2 0 0116.138 21H7.862a2 2 0 01-1.995-1.858L5 7m5 4v6m4-6v6m1-10V4a1 1 0 00-1-1h-4a1 1 0 00-1 1v3M4 7h16"/></svg>
          </button>
        </div>
      `;
      
      // Attach event handler to delete button
      item.querySelector('.btn-remove').addEventListener('click', async (e) => {
        const email = e.currentTarget.getAttribute('data-email');
        if (confirm(`Are you sure you want to disconnect account: ${email}? This will remove its capacity from G-Stack pool.`)) {
          await unlinkAccount(email);
        }
      });
      
      accountsListContainer.appendChild(item);
    });
  }

  // Always append the dashed "Connect New Account" button at the bottom of the list
  const addBtn = document.createElement('div');
  addBtn.className = 'btn-add-account-dashed';
  addBtn.innerHTML = `
    <svg width="14" height="14" fill="none" stroke="currentColor" stroke-width="2.5" viewBox="0 0 24 24"><path d="M12 4v16m8-8H4"/></svg>
    Connect New Account
  `;
  addBtn.addEventListener('click', () => {
    const loginUrl = `http://${appConfig.addr}/auth/login`;
    if (window.api) {
      window.api.openExternal(loginUrl);
    } else {
      window.open(loginUrl, '_blank');
    }
  });
  accountsListContainer.appendChild(addBtn);
}

// Delete account from Go API
async function unlinkAccount(email) {
  try {
    const response = await fetch(`http://${appConfig.addr}/accounts/delete?email=${encodeURIComponent(email)}`, {
      method: 'POST'
    });
    if (!response.ok) throw new Error('Unlink request failed');
    await fetchStatus();
  } catch (error) {
    alert(`Failed to unlink account: ${error.message}`);
  }
}

// Settings Modal controls
async function openSettingsModal() {
  await loadConfig(); // Refresh config from file system
  if (window.api && window.api.getStartupStatus) {
    try {
      const startupEnabled = await window.api.getStartupStatus();
      document.getElementById('input-startup').checked = !!startupEnabled;
    } catch (err) {
      console.error('Failed to fetch startup status:', err);
    }
  }
  settingsModal.classList.add('active');
}

function closeSettingsModal() {
  settingsModal.classList.remove('active');
}

if (btnSettings) btnSettings.addEventListener('click', openSettingsModal);
if (btnCloseSettings) btnCloseSettings.addEventListener('click', closeSettingsModal);
if (btnCancelSettings) btnCancelSettings.addEventListener('click', closeSettingsModal);

if (btnSaveSettings) {
  btnSaveSettings.addEventListener('click', async () => {
    const newConfig = {
      addr: document.getElementById('input-addr').value.trim(),
      username: document.getElementById('input-username').value.trim(),
      password: document.getElementById('input-password').value.trim(),
      client_id: document.getElementById('input-client-id').value.trim(),
      client_secret: document.getElementById('input-client-secret').value.trim()
    };
    
    if (!newConfig.addr || !newConfig.username || !newConfig.password) {
      alert('Error: Daemon Address/Port, WebDAV Username, and Password are required.');
      return;
    }
    
    btnSaveSettings.disabled = true;
    btnSaveSettings.textContent = 'Saving & Restarting...';
    
    try {
      if (window.api && window.api.saveConfig) {
        const result = await window.api.saveConfig(newConfig);
        if (result.success) {
          if (window.api.setStartupStatus) {
            const startupEnabled = document.getElementById('input-startup').checked;
            await window.api.setStartupStatus(startupEnabled);
          }
          closeSettingsModal();
          
          // Update status indicator to reloading
          daemonStatusText.textContent = 'Rebooting Daemon...';
          daemonStatusText.parentElement.style.background = 'rgba(255, 152, 0, 0.08)';
          daemonStatusText.parentElement.style.color = '#ff9800';
          daemonStatusText.parentElement.querySelector('.status-indicator').style.backgroundColor = '#ff9800';
          daemonStatusText.parentElement.querySelector('.status-indicator').style.boxShadow = '0 0 10px #ff9800';
          
          // Wait 2.5 seconds, reload UI configs and re-query status on new port
          setTimeout(async () => {
            await loadConfig();
            await fetchStatus();
            btnSaveSettings.disabled = false;
            btnSaveSettings.textContent = 'Save & Restart Daemon';
          }, 2500);
        } else {
          alert(`Failed to save config: ${result.error}`);
          btnSaveSettings.disabled = false;
          btnSaveSettings.textContent = 'Save & Restart Daemon';
        }
      } else {
        alert('Configuration API not available.');
        btnSaveSettings.disabled = false;
        btnSaveSettings.textContent = 'Save & Restart Daemon';
      }
    } catch (err) {
      alert(`Error saving configuration: ${err.message}`);
      btnSaveSettings.disabled = false;
      btnSaveSettings.textContent = 'Save & Restart Daemon';
    }
  });
}

// Main actions Event Listeners
btnMountDrive.addEventListener('click', () => {
  const mountUrl = `dav://${appConfig.username}:${appConfig.password}@${appConfig.addr}/G-Stack/`;
  if (window.api) {
    window.api.openExternal(mountUrl);
  } else {
    window.open(mountUrl, '_blank');
  }
});

btnCopyWebdav.addEventListener('click', () => {
  const url = webdavUrlText.textContent;
  navigator.clipboard.writeText(url).then(() => {
    const originalText = btnCopyWebdav.innerHTML;
    btnCopyWebdav.innerHTML = `
      <svg width="18" height="18" fill="none" stroke="var(--color-green)" stroke-width="2" viewBox="0 0 24 24"><path d="M5 13l4 4L19 7"/></svg>
      Copied!
    `;
    setTimeout(() => {
      btnCopyWebdav.innerHTML = originalText;
    }, 2000);
  }).catch(err => {
    console.error('Failed to copy text:', err);
  });
});

// Custom close button to minimize/hide to tray
const btnCloseTray = document.getElementById('btn-close-tray');
btnCloseTray.addEventListener('click', () => {
  if (window.api && window.api.hideToTray) {
    window.api.hideToTray();
  }
});

// Toggle password visibility click listener
const btnTogglePasswordView = document.getElementById('btn-toggle-password-view');
if (btnTogglePasswordView) {
  btnTogglePasswordView.addEventListener('click', () => {
    isPasswordVisible = !isPasswordVisible;
    updateCredsDisplay();
    
    // Switch the SVG path between eye and eye-off depending on state
    if (isPasswordVisible) {
      btnTogglePasswordView.innerHTML = `
        <svg width="14" height="14" fill="none" stroke="currentColor" stroke-width="2.2" viewBox="0 0 24 24">
          <path d="M13.875 18.825A10.05 10.05 0 0112 19c-4.478 0-8.268-2.943-9.543-7a9.97 9.97 0 011.563-3.029m5.858.908a3 3 0 114.243 4.243M9.878 9.878l4.242 4.242M9.88 9.88L3 3m18 18l-7-7-7-7" />
        </svg>
      `;
    } else {
      btnTogglePasswordView.innerHTML = `
        <svg width="14" height="14" fill="none" stroke="currentColor" stroke-width="2.2" viewBox="0 0 24 24">
          <path d="M15 12a3 3 0 11-6 0 3 3 0 016 0z" />
          <path d="M2.458 12C3.732 7.943 7.523 5 12 5c4.478 0 8.268 2.943 9.542 7-1.274 4.057-5.064 7-9.542 7-4.477 0-8.268-2.943-9.542-7z" />
        </svg>
      `;
    }
  });
}

// Fetch active uploads from Go VFS engine
async function fetchActiveUploads() {
  try {
    const response = await fetch(`http://${appConfig.addr}/uploads`);
    if (!response.ok) throw new Error('Failed to fetch uploads progress');
    
    const uploads = await response.json();
    renderUploads(uploads || []);
  } catch (err) {
    console.error('Failed to fetch active uploads:', err);
    // Show offline status on active uploads
    const uploadsListContainer = document.getElementById('uploads-list-container');
    const uploadsCountText = document.getElementById('uploads-count');
    if (uploadsListContainer && uploadsCountText) {
      uploadsCountText.textContent = 'Offline';
      uploadsListContainer.innerHTML = `
        <div class="empty-state">
          <p style="color: var(--color-red)">Connection offline</p>
          <p class="sub-empty">Unable to retrieve uploads status from daemon.</p>
        </div>
      `;
    }
  }
}

// Render active uploads list
function renderUploads(uploads) {
  const uploadsCard = document.getElementById('uploads-card');
  const uploadsCountText = document.getElementById('uploads-count');
  const uploadsListContainer = document.getElementById('uploads-list-container');
  
  if (!uploadsCard || !uploadsCountText || !uploadsListContainer) return;
  
  // Make sure card is visible
  uploadsCard.style.display = 'flex';
  
  if (uploads.length === 0) {
    uploadsCountText.textContent = '0 Active';
    uploadsListContainer.innerHTML = `
      <div class="empty-state">
        <p>No active uploads</p>
        <p class="sub-empty">Files copied to your mounted drive will appear here while uploading.</p>
      </div>
    `;
    return;
  }
  
  uploadsCountText.textContent = `${uploads.length} Uploading`;
  
  uploadsListContainer.innerHTML = '';
  
  uploads.forEach(upload => {
    const percent = upload.total_size > 0 ? Math.round((upload.uploaded_size / upload.total_size) * 100) : 0;
    
    const item = document.createElement('div');
    item.className = 'upload-item';
    item.innerHTML = `
      <div class="upload-meta">
        <span class="upload-filename" title="${upload.name}">${upload.name}</span>
        <span class="upload-stats">${formatBytes(upload.uploaded_size)} / ${formatBytes(upload.total_size)}</span>
      </div>
      <div class="upload-progress-wrapper">
        <div class="upload-progress-bar-bg">
          <div class="upload-progress-bar-fill" style="width: ${percent}%"></div>
        </div>
        <span class="upload-percentage">${percent}%</span>
      </div>
    `;
    uploadsListContainer.appendChild(item);
  });
}

// Initialize app config and start polling status
async function init() {
  await loadConfig();
  await fetchStatus();
  setInterval(fetchStatus, POLLING_INTERVAL);
}

init();
