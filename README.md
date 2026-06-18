# G-Stack 🚀

**G-Stack** is a premium desktop application that aggregates multiple Google Drive accounts into a single, unified virtual storage pool. It exposes this pooled storage as a secure local WebDAV server, allowing you to mount, read, and write files directly from your native file manager (such as Dolphin or Nautilus) while managing everything through a gorgeous, glassmorphic desktop GUI.

---

## 🌟 Key Features

* 🔄 **Unified Storage Pooling:** Combine the capacities of multiple Google Drive accounts (e.g., 3 x 15 GB accounts = 45 GB unified pool).
* ⚡ **Dynamic Chunked Distribution:** Files are automatically split into 10 MB chunks and distributed dynamically to the connected Google accounts based on which account has the most free space.
* 📂 **Native File Manager Integration:** Mounts natively in Linux/Windows file managers (like Dolphin, Nautilus, etc.) under the custom name `G-Stack` instead of a generic local IP.
* 🎨 **Premium Glassmorphic GUI:** A stunning desktop dashboard styled with custom neon-dark styling, a real-time storage visualizer ring, and simple account management.
* 🔒 **Local-First Privacy:** All credentials, refresh tokens, and filesystem metadata are stored locally in a secure SQLite database (`gstack.db`). No external cloud databases or intermediaries.
* 🛠️ **Wayland Native Window Mapping:** Built-in `.desktop` configuration and WMClass matching to display high-quality desktop launcher icons on Linux Wayland (GNOME, KDE Plasma, Hyprland).

---

## 🛠️ Tech Stack

* **Backend:** Go (Golang) + `golang.org/x/net/webdav` + Google OAuth 2.0 API.
* **Frontend:** Electron (HTML, CSS, JavaScript) with custom Glassmorphism styling.
* **Database:** SQLite (`gstack.db`) for tracking accounts and virtual directory tree nodes.

---

## ⚙️ Architecture

```mermaid
graph TD
    A[Electron Frontend GUI] -- "IPC / REST API (port 8080)" --> B[Go VFS Backend Daemon]
    B -- "Reads/Writes Metadata" --> C[(Local SQLite db)]
    B -- "Exposes WebDAV Server" --> D[Linux File Manager (Dolphin/Nautilus)]
    B -- "Uploads/Downloads Chunks" --> E[Google Drive Account 1]
    B -- "Uploads/Downloads Chunks" --> F[Google Drive Account 2]
    B -- "Uploads/Downloads Chunks" --> G[Google Drive Account n]
```

---

## 🚀 How to Run Locally

### Prerequisites
1. **Go** (v1.20+)
2. **Node.js** & **NPM**

### Setup Instructions

1. **Clone the Repository:**
   ```bash
   git clone https://github.com/yourusername/g-stack.git
   cd g-stack
   ```

2. **Configure Google API Credentials:**
   Create a `config.json` in the root folder with your Google Client ID and Secret (redirect URI must be set to `http://localhost:8080/auth/callback` in Google Cloud Console):
   ```json
   {
     "client_id": "YOUR_GOOGLE_CLIENT_ID",
     "client_secret": "YOUR_GOOGLE_CLIENT_SECRET",
     "addr": "localhost:8080",
     "username": "admin",
     "password": "YOUR_SECURE_PASSWORD",
     "db_path": "gstack.db",
     "temp_dir": "./temp"
   }
   ```

3. **Install Dependencies:**
   ```bash
   npm install
   ```

4. **Compile the Go Daemon:**
   ```bash
   go build -o gstack .
   ```

5. **Start the Application:**
   ```bash
   npm start
   ```

---

## 📄 License
This project is licensed under the MIT License - see the LICENSE file for details.
