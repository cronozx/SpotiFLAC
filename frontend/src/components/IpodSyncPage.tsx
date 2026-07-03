import { useEffect, useRef, useState } from "react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { Badge } from "@/components/ui/badge";
import { Checkbox } from "@/components/ui/checkbox";
import { Progress } from "@/components/ui/progress";
import { Spinner } from "@/components/ui/spinner";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Smartphone, HardDrive, Music, Heart, ListMusic, Save, PlugZap, Unplug, RefreshCw, Search, Download, StopCircle, CheckCircle2, XCircle, Info, RotateCw } from "lucide-react";
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
    const [syncResult, setSyncResult] = useState<SyncResult | null>(null);

    const pollRef = useRef<ReturnType<typeof setInterval> | null>(null);
    const logContainerRef = useRef<HTMLDivElement | null>(null);

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
    }, [log]);

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

    return (<div className="space-y-6">
        <div className="flex items-center gap-3">
            <h1 className="text-2xl font-bold">iPod Sync</h1>
            {connected && (<Badge className="gap-1 border-transparent bg-green-600 text-white hover:bg-green-600">
                <CheckCircle2 className="h-3 w-3"/>
                Connected
            </Badge>)}
        </div>

        <Card>
            <CardHeader>
                <div className="flex items-center justify-between gap-3">
                    <div className="space-y-1">
                        <CardTitle className="flex items-center gap-2">
                            <Music className="h-4 w-4"/>
                            Spotify Connection
                        </CardTitle>
                        <CardDescription>
                            Connect your own Spotify app to read your library.
                        </CardDescription>
                    </div>
                    {connected ? (<Badge className="gap-1 border-transparent bg-green-600 text-white hover:bg-green-600">
                        <CheckCircle2 className="h-3 w-3"/>
                        Connected
                    </Badge>) : (<Badge variant="secondary" className="gap-1">
                        <XCircle className="h-3 w-3"/>
                        Not Connected
                    </Badge>)}
                </div>
            </CardHeader>
            <CardContent className="space-y-4">
                <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
                    <div className="space-y-2">
                        <Label htmlFor="spotify-client-id">Client ID</Label>
                        <Input id="spotify-client-id" value={clientId} onChange={(e) => setClientId(e.target.value)} placeholder="Your Spotify app Client ID" autoComplete="off"/>
                    </div>
                    <div className="space-y-2">
                        <Label htmlFor="spotify-client-secret">Client Secret</Label>
                        <Input id="spotify-client-secret" type="password" value={clientSecret} onChange={(e) => setClientSecret(e.target.value)} placeholder="Your Spotify app Client Secret" autoComplete="off"/>
                    </div>
                </div>

                <div className="flex items-start gap-2 rounded-md border bg-muted/30 p-3 text-sm text-muted-foreground">
                    <Info className="mt-0.5 h-4 w-4 shrink-0"/>
                    <span>
                        Register the redirect URI{" "}
                        <code className="rounded bg-muted px-1 py-0.5 font-mono text-xs text-foreground">{REDIRECT_URI}</code>{" "}
                        in your Spotify Developer dashboard for the app whose credentials you use here.
                    </span>
                </div>

                <div className="flex flex-wrap gap-2">
                    <Button onClick={handleSaveCredentials} disabled={savingCredentials} className="gap-1.5">
                        {savingCredentials ? <Spinner /> : <Save className="h-4 w-4"/>}
                        Save Credentials
                    </Button>
                    {connected ? (<Button variant="outline" onClick={handleDisconnect} className="gap-1.5">
                        <Unplug className="h-4 w-4"/>
                        Disconnect
                    </Button>) : (<Button variant="outline" onClick={handleConnect} disabled={connecting} className="gap-1.5">
                        {connecting ? <Spinner /> : <PlugZap className="h-4 w-4"/>}
                        {connecting ? "Waiting for authorization..." : "Connect"}
                    </Button>)}
                </div>
            </CardContent>
        </Card>

        <Card>
            <CardHeader>
                <div className="flex items-center justify-between gap-3">
                    <div className="space-y-1">
                        <CardTitle className="flex items-center gap-2">
                            <Smartphone className="h-4 w-4"/>
                            iPod
                        </CardTitle>
                        <CardDescription>
                            Detect your connected Rockbox iPod.
                        </CardDescription>
                    </div>
                    <Button variant="outline" size="sm" onClick={handleDetectIpod} disabled={detecting} className="gap-1.5">
                        {detecting ? <Spinner /> : <Search className="h-4 w-4"/>}
                        Detect iPod
                    </Button>
                </div>
            </CardHeader>
            <CardContent>
                {ipod ? (<div className="space-y-4">
                    <div className="flex flex-wrap items-center gap-2">
                        <span className="text-base font-semibold">{ipod.name || "iPod"}</span>
                        {ipod.is_rockbox ? (<Badge className="gap-1 border-transparent bg-green-600 text-white hover:bg-green-600">
                            <CheckCircle2 className="h-3 w-3"/>
                            Rockbox
                        </Badge>) : (<Badge variant="secondary" className="gap-1">
                            <XCircle className="h-3 w-3"/>
                            Rockbox Not Detected
                        </Badge>)}
                    </div>
                    <div className="grid grid-cols-1 gap-3 text-sm sm:grid-cols-2">
                        <div className="space-y-1">
                            <p className="text-xs uppercase tracking-wide text-muted-foreground">Mount Path</p>
                            <p className="break-all font-mono text-xs">{ipod.mount_path || "-"}</p>
                        </div>
                        <div className="space-y-1">
                            <p className="text-xs uppercase tracking-wide text-muted-foreground">Music Path</p>
                            <p className="break-all font-mono text-xs">{ipod.music_path || "-"}</p>
                        </div>
                    </div>
                    <div className="space-y-2">
                        <div className="flex items-center justify-between text-sm">
                            <span className="flex items-center gap-1.5 text-muted-foreground">
                                <HardDrive className="h-4 w-4"/>
                                Storage
                            </span>
                            <span className="font-mono text-xs">
                                {formatBytes(ipod.total_bytes - ipod.free_bytes)} used of {formatBytes(ipod.total_bytes)}
                                {" "}({formatBytes(ipod.free_bytes)} free)
                            </span>
                        </div>
                        <Progress value={ipod.total_bytes > 0 ? ((ipod.total_bytes - ipod.free_bytes) / ipod.total_bytes) * 100 : 0}/>
                    </div>
                </div>) : (<div className="flex flex-col items-center justify-center gap-3 py-10 text-center text-muted-foreground">
                    <div className="rounded-full bg-muted/50 p-4 ring-8 ring-muted/20">
                        <Smartphone className="h-8 w-8 opacity-40"/>
                    </div>
                    <div className="space-y-1">
                        <p className="font-medium text-foreground/80">{ipodError ? "No iPod found" : "No iPod detected"}</p>
                        <p className="text-sm">{ipodError || "Connect your Rockbox iPod and click Detect iPod."}</p>
                    </div>
                </div>)}
            </CardContent>
        </Card>

        <Card>
            <CardHeader>
                <div className="space-y-1">
                    <CardTitle className="flex items-center gap-2">
                        <ListMusic className="h-4 w-4"/>
                        What to Sync
                    </CardTitle>
                    <CardDescription>
                        Choose which parts of your library to copy to the iPod.
                    </CardDescription>
                </div>
            </CardHeader>
            <CardContent className="space-y-5">
                <div className="flex items-center justify-between gap-3">
                    <Label htmlFor="include-liked-songs" className="flex cursor-pointer items-center gap-2 font-normal">
                        <Heart className="h-4 w-4"/>
                        Include Liked Songs
                    </Label>
                    <Switch id="include-liked-songs" checked={settings.includeLikedSongs} onCheckedChange={(checked) => void persistSettings({ ...settings, includeLikedSongs: checked })}/>
                </div>

                <div className="flex items-center justify-between gap-3">
                    <Label htmlFor="auto-sync-on-launch" className="flex cursor-pointer items-center gap-2 font-normal">
                        <RotateCw className="h-4 w-4"/>
                        Sync Automatically on Launch
                    </Label>
                    <Switch id="auto-sync-on-launch" checked={settings.autoSyncOnLaunch} onCheckedChange={(checked) => void persistSettings({ ...settings, autoSyncOnLaunch: checked })}/>
                </div>

                <div className="space-y-3">
                    <div className="flex items-center justify-between gap-2">
                        <Label className="font-normal">Playlists</Label>
                        <Button variant="outline" size="sm" onClick={handleLoadPlaylists} disabled={loadingPlaylists || !connected} className="gap-1.5">
                            {loadingPlaylists ? <Spinner /> : <RefreshCw className="h-4 w-4"/>}
                            Load Playlists
                        </Button>
                    </div>
                    {!connected ? (<p className="text-sm text-muted-foreground">Connect Spotify to load your playlists.</p>) : playlists.length === 0 ? (<p className="text-sm text-muted-foreground">
                        {loadingPlaylists ? "Loading playlists..." : "No playlists loaded yet. Click Load Playlists."}
                    </p>) : (<div className="max-h-72 space-y-1 overflow-y-auto rounded-md border p-1">
                        {playlists.map((playlist) => {
                            const checked = settings.selectedPlaylistIds.includes(playlist.id);
                            return (<label key={playlist.id} className="flex cursor-pointer items-center gap-3 rounded-md px-2 py-2 transition-colors hover:bg-muted/50">
                                <Checkbox className="shrink-0" checked={checked} onCheckedChange={(value) => togglePlaylist(playlist.id, value === true)}/>
                                <div className="min-w-0 flex-1">
                                    <p className="truncate text-sm font-medium">{playlist.name}</p>
                                    <p className="truncate text-xs text-muted-foreground">{playlist.owner}</p>
                                </div>
                                <Badge variant="secondary" className="font-mono">
                                    {playlist.track_count.toLocaleString("en-US")}
                                </Badge>
                            </label>);
                        })}
                    </div>)}
                </div>
            </CardContent>
        </Card>

        <Card>
            <CardHeader>
                <div className="space-y-1">
                    <CardTitle className="flex items-center gap-2">
                        <Download className="h-4 w-4"/>
                        Sync
                    </CardTitle>
                    <CardDescription>
                        Download your selected library as FLAC and copy it to the iPod.
                    </CardDescription>
                </div>
            </CardHeader>
            <CardContent className="space-y-4">
                <div className="flex flex-wrap gap-2">
                    <Button size="lg" onClick={handleSync} disabled={syncing || !connected} className="gap-2">
                        {syncing ? <Spinner /> : <Download className="h-5 w-5"/>}
                        {syncing ? "Syncing..." : "Sync now"}
                    </Button>
                    {syncing && (<Button size="lg" variant="destructive" onClick={handleCancelSync} className="gap-2">
                        <StopCircle className="h-5 w-5"/>
                        Cancel
                    </Button>)}
                </div>

                {(syncing || progress > 0 || statusLine) && (<div className="space-y-2">
                    <div className="flex items-center justify-between text-sm">
                        <span className="truncate text-muted-foreground">{statusLine || "Preparing..."}</span>
                        <span className="shrink-0 font-mono text-xs">{Math.round(progress)}%</span>
                    </div>
                    <Progress value={progress}/>
                </div>)}

                {log.length > 0 && (<div ref={logContainerRef} className="max-h-56 overflow-y-auto rounded-md border bg-muted/30 p-3 font-mono text-xs leading-relaxed">
                    {log.map((line, index) => (<div key={index} className="whitespace-pre-wrap break-all text-muted-foreground">
                        {line}
                    </div>))}
                </div>)}

                {syncResult && (<div className={`rounded-md border p-4 ${syncResult.failed > 0 ? "border-amber-500/40 bg-amber-500/10" : "border-green-500/40 bg-green-500/10"}`}>
                    <p className="flex items-center gap-2 text-sm font-semibold">
                        {syncResult.failed > 0 ? (<XCircle className="h-4 w-4 text-amber-600 dark:text-amber-400"/>) : (<CheckCircle2 className="h-4 w-4 text-green-600 dark:text-green-400"/>)}
                        {syncResult.message || "Sync complete"}
                    </p>
                    <div className="mt-3 flex flex-wrap gap-2">
                        <Badge variant="secondary" className="font-mono">Synced {syncResult.synced}</Badge>
                        <Badge variant="secondary" className="font-mono">Skipped {syncResult.skipped}</Badge>
                        <Badge variant="secondary" className="font-mono">Failed {syncResult.failed}</Badge>
                        <Badge variant="secondary" className="font-mono">Total {syncResult.total}</Badge>
                    </div>
                </div>)}
            </CardContent>
        </Card>
    </div>);
}
