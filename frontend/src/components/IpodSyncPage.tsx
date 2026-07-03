import { useEffect, useRef, useState } from "react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { Badge } from "@/components/ui/badge";
import { Checkbox } from "@/components/ui/checkbox";
import { Progress } from "@/components/ui/progress";
import { Spinner } from "@/components/ui/spinner";
import { Card, CardContent } from "@/components/ui/card";
import { HardDrive, Heart, RotateCw, PlugZap, Unplug, Save, RefreshCw, Search, Download, StopCircle, CheckCircle2, ChevronDown, Settings2 } from "lucide-react";
import { SaveSpotifyCredentials, GetSpotifyCredentials, ConnectSpotify, SpotifyConnectionStatus, DisconnectSpotify, DetectIpod, GetIpodSyncSettings, SaveIpodSyncSettings, ListSpotifyPlaylists, SyncLibraryToIpod, CancelIpodSync } from "../../wailsjs/go/main/App";
import { EventsOn, EventsOff } from "../../wailsjs/runtime/runtime";
import { toastWithSound as toast } from "@/lib/toast-with-sound";

interface SpotifyCredentials {
    clientId: string;
    clientSecret: string;
    connected: boolean;
}
interface IpodInfo {
    name: string;
    mount_path: string;
    music_path: string;
    connected: boolean;
    is_rockbox: boolean;
    free_bytes: number;
    total_bytes: number;
}
interface IpodSyncSettings {
    autoSyncOnLaunch: boolean;
    includeLikedSongs: boolean;
    selectedPlaylistIds: string[];
}
interface SpotifyPlaylist {
    id: string;
    name: string;
    url: string;
    track_count: number;
    owner: string;
}
interface SyncResult {
    synced: number;
    skipped: number;
    failed: number;
    total: number;
    message: string;
}

const DEFAULT_SYNC_SETTINGS: IpodSyncSettings = {
    autoSyncOnLaunch: false,
    includeLikedSongs: true,
    selectedPlaylistIds: [],
};
const REDIRECT_URI = "http://127.0.0.1:8888/callback";
const CONNECT_POLL_INTERVAL_MS = 1500;
const CONNECT_POLL_MAX_ATTEMPTS = 60;

const formatBytes = (bytes: number): string => {
    if (!bytes || bytes <= 0)
        return "0 GB";
    return `${(bytes / 1024 ** 3).toFixed(1)} GB`;
};

export function IpodSyncPage() {
    const [clientId, setClientId] = useState("");
    const [clientSecret, setClientSecret] = useState("");
    const [connected, setConnected] = useState(false);
    const [savingCredentials, setSavingCredentials] = useState(false);
    const [connecting, setConnecting] = useState(false);
    const [showCredentials, setShowCredentials] = useState(false);

    const [ipod, setIpod] = useState<IpodInfo | null>(null);
    const [detecting, setDetecting] = useState(false);
    const [ipodError, setIpodError] = useState<string | null>(null);

    const [settings, setSettings] = useState<IpodSyncSettings>(DEFAULT_SYNC_SETTINGS);
    const [playlists, setPlaylists] = useState<SpotifyPlaylist[]>([]);
    const [loadingPlaylists, setLoadingPlaylists] = useState(false);

    const [syncing, setSyncing] = useState(false);
    const [progress, setProgress] = useState(0);
    const [statusLine, setStatusLine] = useState("");
    const [log, setLog] = useState<string[]>([]);
    const [showLog, setShowLog] = useState(false);
    const [syncResult, setSyncResult] = useState<SyncResult | null>(null);

    const pollRef = useRef<ReturnType<typeof setInterval> | null>(null);
    const logContainerRef = useRef<HTMLDivElement | null>(null);

    const showCredentialFields = !connected || showCredentials;

    useEffect(() => {
        const loadInitial = async () => {
            try {
                const creds = (await GetSpotifyCredentials()) as SpotifyCredentials;
                setClientId(creds.clientId || "");
                setClientSecret(creds.clientSecret || "");
                setConnected(!!creds.connected);
            }
            catch (err) {
                console.error("Failed to load Spotify credentials:", err);
            }
            try {
                const saved = (await GetIpodSyncSettings()) as IpodSyncSettings;
                setSettings({
                    autoSyncOnLaunch: !!saved.autoSyncOnLaunch,
                    includeLikedSongs: !!saved.includeLikedSongs,
                    selectedPlaylistIds: saved.selectedPlaylistIds || [],
                });
            }
            catch (err) {
                console.error("Failed to load iPod sync settings:", err);
            }
        };
        void loadInitial();
    }, []);

    useEffect(() => {
        EventsOn("ipod-sync:status", (status: string) => {
            setStatusLine(status);
        });
        EventsOn("ipod-sync:progress", (value: number) => {
            setProgress(value);
        });
        EventsOn("ipod-sync:log", (line: string) => {
            setLog((prev) => [...prev, line]);
        });
        return () => {
            EventsOff("ipod-sync:status");
            EventsOff("ipod-sync:progress");
            EventsOff("ipod-sync:log");
        };
    }, []);

    useEffect(() => {
        return () => {
            if (pollRef.current) {
                clearInterval(pollRef.current);
                pollRef.current = null;
            }
        };
    }, []);

    useEffect(() => {
        const container = logContainerRef.current;
        if (container) {
            container.scrollTop = container.scrollHeight;
        }
    }, [log, showLog]);

    const persistSettings = async (next: IpodSyncSettings) => {
        setSettings(next);
        try {
            await SaveIpodSyncSettings(next);
        }
        catch (err) {
            console.error("Failed to save iPod sync settings:", err);
            toast.error(`Failed to save sync settings: ${err}`);
        }
    };

    const handleSaveCredentials = async () => {
        if (!clientId.trim() || !clientSecret.trim()) {
            toast.error("Enter both Client ID and Client Secret");
            return;
        }
        setSavingCredentials(true);
        try {
            await SaveSpotifyCredentials(clientId.trim(), clientSecret.trim());
            toast.success("Spotify credentials saved");
        }
        catch (err) {
            console.error("Failed to save Spotify credentials:", err);
            toast.error(`Failed to save credentials: ${err}`);
        }
        finally {
            setSavingCredentials(false);
        }
    };

    const startConnectionPolling = () => {
        if (pollRef.current) {
            clearInterval(pollRef.current);
            pollRef.current = null;
        }
        let attempts = 0;
        pollRef.current = setInterval(async () => {
            attempts += 1;
            try {
                const isConnected = await SpotifyConnectionStatus();
                if (isConnected) {
                    setConnected(true);
                    setConnecting(false);
                    setShowCredentials(false);
                    if (pollRef.current) {
                        clearInterval(pollRef.current);
                        pollRef.current = null;
                    }
                    toast.success("Connected to Spotify");
                    return;
                }
            }
            catch (err) {
                console.error("Failed to check Spotify connection status:", err);
            }
            if (attempts >= CONNECT_POLL_MAX_ATTEMPTS) {
                setConnecting(false);
                if (pollRef.current) {
                    clearInterval(pollRef.current);
                    pollRef.current = null;
                }
                toast.error("Timed out waiting for Spotify authorization");
            }
        }, CONNECT_POLL_INTERVAL_MS);
    };

    const handleConnect = async () => {
        if (!clientId.trim() || !clientSecret.trim()) {
            toast.error("Save your Client ID and Client Secret first");
            return;
        }
        setConnecting(true);
        try {
            await ConnectSpotify();
            startConnectionPolling();
        }
        catch (err) {
            console.error("Failed to start Spotify connection:", err);
            setConnecting(false);
            toast.error(`Failed to connect to Spotify: ${err}`);
        }
    };

    const handleDisconnect = async () => {
        try {
            await DisconnectSpotify();
            setConnected(false);
            setPlaylists([]);
            toast.success("Disconnected from Spotify");
        }
        catch (err) {
            console.error("Failed to disconnect from Spotify:", err);
            toast.error(`Failed to disconnect: ${err}`);
        }
    };

    const handleDetectIpod = async () => {
        setDetecting(true);
        setIpodError(null);
        try {
            const info = (await DetectIpod()) as IpodInfo;
            if (info && info.connected) {
                setIpod(info);
            }
            else {
                setIpod(null);
                setIpodError("No iPod detected. Connect your device and try again.");
            }
        }
        catch (err) {
            console.error("Failed to detect iPod:", err);
            setIpod(null);
            setIpodError(`Failed to detect iPod: ${err}`);
        }
        finally {
            setDetecting(false);
        }
    };

    const handleLoadPlaylists = async () => {
        setLoadingPlaylists(true);
        try {
            const items = (await ListSpotifyPlaylists()) as SpotifyPlaylist[];
            setPlaylists(items || []);
        }
        catch (err) {
            console.error("Failed to load playlists:", err);
            toast.error(`Failed to load playlists: ${err}`);
        }
        finally {
            setLoadingPlaylists(false);
        }
    };

    const togglePlaylist = (id: string, checked: boolean) => {
        const nextIds = checked
            ? Array.from(new Set([...settings.selectedPlaylistIds, id]))
            : settings.selectedPlaylistIds.filter((existing) => existing !== id);
        void persistSettings({ ...settings, selectedPlaylistIds: nextIds });
    };

    const handleSync = async () => {
        setSyncing(true);
        setSyncResult(null);
        setProgress(0);
        setStatusLine("");
        setLog([]);
        try {
            const result = (await SyncLibraryToIpod()) as SyncResult;
            setSyncResult(result);
            if (result.failed > 0) {
                toast.error(result.message || `Sync finished with ${result.failed} failures`);
            }
            else {
                toast.success(result.message || `Synced ${result.synced} tracks`);
            }
        }
        catch (err) {
            console.error("Failed to sync library:", err);
            toast.error(`Sync failed: ${err}`);
        }
        finally {
            setSyncing(false);
        }
    };

    const handleCancelSync = async () => {
        try {
            await CancelIpodSync();
        }
        catch (err) {
            console.error("Failed to cancel sync:", err);
            toast.error(`Failed to cancel sync: ${err}`);
        }
    };

    const usedBytes = ipod ? Math.max(0, ipod.total_bytes - ipod.free_bytes) : 0;
    const usedPercent = ipod && ipod.total_bytes > 0 ? (usedBytes / ipod.total_bytes) * 100 : 0;

    return (<div className="mx-auto max-w-2xl space-y-4">
        <div>
            <h1 className="text-2xl font-bold">iPod Sync</h1>
            <p className="text-sm text-muted-foreground">Copy your Spotify library to your Rockbox iPod as FLAC.</p>
        </div>

        {/* Setup: Spotify + iPod */}
        <Card>
            <CardContent className="divide-y pt-6">
                {/* Spotify */}
                <div className="space-y-3 pb-4">
                    <div className="flex items-center justify-between gap-2">
                        <span className="text-sm font-medium">Spotify</span>
                        <div className="flex items-center gap-2">
                            {connected && (<Badge className="gap-1 border-transparent bg-green-600 text-white hover:bg-green-600">
                                <CheckCircle2 className="h-3 w-3"/>
                                Connected
                            </Badge>)}
                            {connected && (<Button variant="ghost" size="sm" className="h-7 gap-1.5 text-muted-foreground" onClick={() => setShowCredentials((v) => !v)}>
                                <Settings2 className="h-3.5 w-3.5"/>
                                Credentials
                            </Button>)}
                        </div>
                    </div>

                    {showCredentialFields && (<>
                        <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
                            <Input aria-label="Client ID" value={clientId} onChange={(e) => setClientId(e.target.value)} placeholder="Client ID" autoComplete="off"/>
                            <Input aria-label="Client Secret" type="password" value={clientSecret} onChange={(e) => setClientSecret(e.target.value)} placeholder="Client Secret" autoComplete="off"/>
                        </div>
                        {!connected && (<p className="text-xs text-muted-foreground">
                            Add the redirect URI{" "}
                            <code className="rounded bg-muted px-1 py-0.5 font-mono text-foreground">{REDIRECT_URI}</code>{" "}
                            to your Spotify app.
                        </p>)}
                    </>)}

                    <div className="flex flex-wrap gap-2">
                        {showCredentialFields && (<Button size="sm" onClick={handleSaveCredentials} disabled={savingCredentials} className="gap-1.5">
                            {savingCredentials ? <Spinner /> : <Save className="h-4 w-4"/>}
                            Save
                        </Button>)}
                        {connected ? (<Button size="sm" variant="outline" onClick={handleDisconnect} className="gap-1.5">
                            <Unplug className="h-4 w-4"/>
                            Disconnect
                        </Button>) : (<Button size="sm" variant="outline" onClick={handleConnect} disabled={connecting} className="gap-1.5">
                            {connecting ? <Spinner /> : <PlugZap className="h-4 w-4"/>}
                            {connecting ? "Waiting…" : "Connect"}
                        </Button>)}
                    </div>
                </div>

                {/* iPod */}
                <div className="space-y-3 pt-4">
                    <div className="flex items-center justify-between gap-2">
                        <span className="text-sm font-medium">iPod</span>
                        <div className="flex items-center gap-2">
                            {ipod && (<Badge variant={ipod.is_rockbox ? undefined : "secondary"} className={ipod.is_rockbox ? "gap-1 border-transparent bg-green-600 text-white hover:bg-green-600" : ""}>
                                {ipod.is_rockbox ? "Rockbox" : "No Rockbox"}
                            </Badge>)}
                            <Button variant="ghost" size="sm" onClick={handleDetectIpod} disabled={detecting} className="h-7 gap-1.5 text-muted-foreground">
                                {detecting ? <Spinner /> : <Search className="h-3.5 w-3.5"/>}
                                {ipod ? "Re-detect" : "Detect"}
                            </Button>
                        </div>
                    </div>

                    {ipod ? (<div className="flex items-center gap-3">
                        <HardDrive className="h-4 w-4 shrink-0 text-muted-foreground"/>
                        <div className="min-w-0 flex-1">
                            <div className="flex items-center justify-between gap-2 text-xs">
                                <span className="truncate font-medium">{ipod.name || "iPod"}</span>
                                <span className="shrink-0 text-muted-foreground">{formatBytes(usedBytes)} / {formatBytes(ipod.total_bytes)}</span>
                            </div>
                            <Progress className="mt-1.5 h-1.5" value={usedPercent}/>
                        </div>
                    </div>) : (<p className="text-xs text-muted-foreground">{ipodError || "Connect your Rockbox iPod and click Detect."}</p>)}
                </div>
            </CardContent>
        </Card>

        {/* What to sync */}
        <Card>
            <CardContent className="space-y-4 pt-6">
                <div className="flex items-center justify-between gap-3">
                    <Label htmlFor="include-liked-songs" className="flex cursor-pointer items-center gap-2 text-sm font-normal">
                        <Heart className="h-4 w-4 text-muted-foreground"/>
                        Include Liked Songs
                    </Label>
                    <Switch id="include-liked-songs" checked={settings.includeLikedSongs} onCheckedChange={(checked) => void persistSettings({ ...settings, includeLikedSongs: checked })}/>
                </div>

                <div className="flex items-center justify-between gap-3">
                    <Label htmlFor="auto-sync-on-launch" className="flex cursor-pointer items-center gap-2 text-sm font-normal">
                        <RotateCw className="h-4 w-4 text-muted-foreground"/>
                        Sync automatically on launch
                    </Label>
                    <Switch id="auto-sync-on-launch" checked={settings.autoSyncOnLaunch} onCheckedChange={(checked) => void persistSettings({ ...settings, autoSyncOnLaunch: checked })}/>
                </div>

                <div className="space-y-2 border-t pt-4">
                    <div className="flex items-center justify-between gap-2">
                        <span className="text-sm font-medium">Playlists{settings.selectedPlaylistIds.length > 0 ? ` · ${settings.selectedPlaylistIds.length} selected` : ""}</span>
                        <Button variant="ghost" size="sm" onClick={handleLoadPlaylists} disabled={loadingPlaylists || !connected} className="h-7 gap-1.5 text-muted-foreground">
                            {loadingPlaylists ? <Spinner /> : <RefreshCw className="h-3.5 w-3.5"/>}
                            {playlists.length > 0 ? "Reload" : "Load"}
                        </Button>
                    </div>
                    {!connected ? (<p className="text-xs text-muted-foreground">Connect Spotify to load your playlists.</p>) : playlists.length === 0 ? (<p className="text-xs text-muted-foreground">
                        {loadingPlaylists ? "Loading…" : "No playlists loaded yet."}
                    </p>) : (<div className="max-h-64 space-y-0.5 overflow-y-auto rounded-md border p-1">
                        {playlists.map((playlist) => {
                            const checked = settings.selectedPlaylistIds.includes(playlist.id);
                            return (<label key={playlist.id} className="flex cursor-pointer items-center gap-3 rounded px-2 py-1.5 transition-colors hover:bg-muted/50">
                                <Checkbox className="shrink-0" checked={checked} onCheckedChange={(value) => togglePlaylist(playlist.id, value === true)}/>
                                <span className="min-w-0 flex-1 truncate text-sm">{playlist.name}</span>
                                <span className="shrink-0 font-mono text-xs text-muted-foreground">{playlist.track_count.toLocaleString("en-US")}</span>
                            </label>);
                        })}
                    </div>)}
                </div>
            </CardContent>
        </Card>

        {/* Sync */}
        <Card>
            <CardContent className="space-y-3 pt-6">
                <div className="flex flex-wrap items-center gap-2">
                    <Button onClick={handleSync} disabled={syncing || !connected} className="gap-2">
                        {syncing ? <Spinner /> : <Download className="h-4 w-4"/>}
                        {syncing ? "Syncing…" : "Sync now"}
                    </Button>
                    {syncing && (<Button variant="destructive" onClick={handleCancelSync} className="gap-2">
                        <StopCircle className="h-4 w-4"/>
                        Cancel
                    </Button>)}
                    {log.length > 0 && (<Button variant="ghost" size="sm" onClick={() => setShowLog((v) => !v)} className="ml-auto h-8 gap-1.5 text-muted-foreground">
                        <ChevronDown className={`h-4 w-4 transition-transform ${showLog ? "rotate-180" : ""}`}/>
                        {showLog ? "Hide log" : "Show log"}
                    </Button>)}
                </div>

                {(syncing || progress > 0 || statusLine) && (<div className="space-y-2">
                    <div className="flex items-center justify-between text-xs">
                        <span className="truncate text-muted-foreground">{statusLine || "Preparing…"}</span>
                        <span className="shrink-0 font-mono">{Math.round(progress)}%</span>
                    </div>
                    <Progress value={progress}/>
                </div>)}

                {showLog && log.length > 0 && (<div ref={logContainerRef} className="max-h-56 overflow-y-auto rounded-md border bg-muted/30 p-3 font-mono text-xs leading-relaxed">
                    {log.map((line, index) => (<div key={index} className="whitespace-pre-wrap break-all text-muted-foreground">
                        {line}
                    </div>))}
                </div>)}

                {syncResult && (<div className={`flex flex-wrap items-center gap-x-4 gap-y-1 rounded-md border px-3 py-2 text-sm ${syncResult.failed > 0 ? "border-amber-500/40 bg-amber-500/10" : "border-green-500/40 bg-green-500/10"}`}>
                    <span className="font-medium">{syncResult.synced} synced</span>
                    <span className="text-muted-foreground">{syncResult.skipped} skipped</span>
                    <span className="text-muted-foreground">{syncResult.failed} failed</span>
                    <span className="text-muted-foreground">of {syncResult.total}</span>
                </div>)}
            </CardContent>
        </Card>
    </div>);
}
