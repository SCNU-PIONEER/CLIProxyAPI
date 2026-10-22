"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import { api, type OAuthProvider, type AuthFile } from "@/lib/api";
import { toast } from "sonner";
import {
  KeyRound,
  ShieldCheck,
  ShieldOff,
  ExternalLink,
  Copy,
} from "lucide-react";

import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Skeleton } from "@/components/ui/skeleton";
import { Spinner } from "@/components/ui/spinner";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";

interface ProviderParamDef {
  key: string;
  label: string;
  placeholder?: string;
  required?: boolean;
  secret?: boolean;
}

/**
 * Providers whose start parameters are supplied by the operator. Everything
 * else is started with just the provider key; the backend appends whatever the
 * provider needs (PKCE verifier, device flow state, ...) itself.
 */
const PROVIDER_PARAMS: Record<string, ProviderParamDef[]> = {
  gitlab: [
    {
      key: "client_id",
      label: "Client ID",
      placeholder: "GitLab OAuth Application ID",
      required: true,
    },
    {
      key: "base_url",
      label: "Base URL",
      placeholder: "https://gitlab.com (default)",
    },
  ],
  kiro: [
    {
      key: "method",
      label: "Login method",
      placeholder: "builder-id (default) or idc",
    },
    {
      key: "flow",
      label: "Flow",
      placeholder: "device (default) or authcode",
    },
    {
      key: "start_url",
      label: "IAM Identity Center start URL (IDC only)",
      placeholder: "https://my-org.awsapps.com/start",
    },
    {
      key: "region",
      label: "Region",
      placeholder: "us-east-1",
    },
  ],
};

type DialogPhase = "starting" | "waiting" | "challenge" | "input" | "done";

function flowLabel(authType: string) {
  switch (authType) {
    case "browser":
    case "authorization_code_pkce":
    case "pkce_polling":
    case "pkce_custom_uri":
      return "PKCE";
    case "google_oauth2":
      return "OAuth2";
    case "aws_builder_id":
      return "Builder ID";
    case "web_oauth":
      return "Web OAuth";
    case "device_code":
      return "Device Code";
    case "token":
      return "Token";
    default:
      return authType;
  }
}

function flowTypeBadge(authType: string) {
  const variant =
    authType === "browser" ||
    authType === "authorization_code_pkce" ||
    authType === "pkce_polling" ||
    authType === "pkce_custom_uri"
      ? "default"
      : authType === "device_code"
        ? "secondary"
        : "outline";
  return <Badge variant={variant}>{flowLabel(authType)}</Badge>;
}

function authStatusBadge(hasAuth: boolean) {
  return hasAuth ? (
    <Badge variant="default" className="gap-1">
      <ShieldCheck className="size-3" />
      Authenticated
    </Badge>
  ) : (
    <Badge variant="outline" className="gap-1 text-muted-foreground">
      <ShieldOff className="size-3" />
      Not Authenticated
    </Badge>
  );
}

export default function OAuthPage() {
  return (
    <div className="flex flex-col gap-6">
      <div className="flex items-center gap-2">
        <KeyRound className="size-5 text-muted-foreground" />
        <h1 className="text-lg font-semibold">OAuth</h1>
      </div>

      <ProvidersTab />
    </div>
  );
}

function ProvidersTab() {
  const [providers, setProviders] = useState<OAuthProvider[]>([]);
  const [authFiles, setAuthFiles] = useState<AuthFile[]>([]);
  const [loading, setLoading] = useState(true);
  const fetchIdRef = useRef(0);

  const [authDialogOpen, setAuthDialogOpen] = useState(false);
  const [authDialogProvider, setAuthDialogProvider] = useState("");
  const [authDialogDisplayName, setAuthDialogDisplayName] = useState("");
  const [phase, setPhase] = useState<DialogPhase>("starting");
  const [authState, setAuthState] = useState("");
  const [authUrl, setAuthUrl] = useState("");
  const [authUserCode, setAuthUserCode] = useState("");
  const [authVerificationUri, setAuthVerificationUri] = useState("");
  const [inputPrompt, setInputPrompt] = useState("");
  const [inputPromptId, setInputPromptId] = useState("");
  const [inputValue, setInputValue] = useState("");
  const [submittingInput, setSubmittingInput] = useState(false);
  const openedUrlRef = useRef("");

  const pollRef = useRef<ReturnType<typeof setInterval> | null>(null);

  const [paramDialogOpen, setParamDialogOpen] = useState(false);
  const [paramDialogProvider, setParamDialogProvider] =
    useState<OAuthProvider | null>(null);
  const [paramDialogValues, setParamDialogValues] = useState<
    Record<string, string>
  >({});

  const fetchData = useCallback(async () => {
    const fetchId = ++fetchIdRef.current;
    try {
      const [provData, authData] = await Promise.all([
        api.oauthFlows.getOAuthProviders(),
        api.authFiles.listAuthFiles(),
      ]);
      if (fetchId === fetchIdRef.current) {
        setProviders(
          [...provData].sort((a, b) =>
            (a.display_name || a.key).localeCompare(b.display_name || b.key),
          ),
        );
        setAuthFiles(authData);
      }
    } catch (err) {
      if (fetchId === fetchIdRef.current) {
        toast.error("Failed to load OAuth providers", {
          description: err instanceof Error ? err.message : undefined,
        });
      }
    } finally {
      if (fetchId === fetchIdRef.current) setLoading(false);
    }
  }, []);

  useEffect(() => {
    fetchData();
  }, [fetchData]);

  const providerHasAuth = useCallback(
    (provider: OAuthProvider) => {
      const names = new Set([provider.key, ...(provider.aliases ?? [])]);
      return authFiles.some(
        (f) => names.has(f.provider) && f.status !== "disabled",
      );
    },
    [authFiles],
  );

  const stopPolling = useCallback(() => {
    if (pollRef.current) {
      clearInterval(pollRef.current);
      pollRef.current = null;
    }
  }, []);

  const resetDialog = useCallback(() => {
    stopPolling();
    setAuthState("");
    setAuthUrl("");
    setAuthUserCode("");
    setAuthVerificationUri("");
    setInputPrompt("");
    setInputPromptId("");
    setInputValue("");
    setSubmittingInput(false);
    openedUrlRef.current = "";
  }, [stopPolling]);

  const closeDialog = useCallback(() => {
    resetDialog();
    setAuthDialogOpen(false);
  }, [resetDialog]);

  /** Applies one poll result to the dialog state machine. */
  const applyStatus = useCallback(
    (displayName: string, status: Awaited<ReturnType<typeof api.oauthFlows.getAuthStatus>>) => {
      if (status.status === "ok") {
        stopPolling();
        setPhase("done");
        setAuthDialogOpen(false);
        toast.success(`${displayName} authenticated successfully`);
        void fetchData();
        return;
      }
      if (status.status === "error") {
        stopPolling();
        setPhase("done");
        setAuthDialogOpen(false);
        toast.error(`Authentication failed for ${displayName}`, {
          description: status.error || status.message,
        });
        return;
      }
      if (status.url) {
        setAuthUrl(status.url);
        if (openedUrlRef.current !== status.url) {
          openedUrlRef.current = status.url;
          window.open(status.url, "_blank", "noopener,noreferrer");
        }
      }
      if (status.user_code) setAuthUserCode(status.user_code);
      if (status.verification_uri) setAuthVerificationUri(status.verification_uri);
      if (status.prompt && status.prompt_id) {
        setPhase("input");
        setInputPrompt(status.prompt);
        setInputPromptId(status.prompt_id);
      } else if (status.status === "challenge") {
        setPhase("challenge");
      } else {
        setPhase("waiting");
      }
    },
    [fetchData, stopPolling],
  );

  const startPolling = useCallback(
    (displayName: string, state: string) => {
      stopPolling();
      pollRef.current = setInterval(async () => {
        try {
          const status = await api.oauthFlows.getAuthStatus(state);
          applyStatus(displayName, status);
        } catch {
          // Keep polling through transient network errors.
        }
      }, 2000);
    },
    [applyStatus, stopPolling],
  );

  const startAuthFlow = useCallback(
    async (provider: OAuthProvider, params?: Record<string, string>) => {
      resetDialog();
      setAuthDialogProvider(provider.key);
      setAuthDialogDisplayName(provider.display_name || provider.key);
      setPhase("starting");
      setAuthDialogOpen(true);

      try {
        const filteredParams = params
          ? Object.fromEntries(
              Object.entries(params).filter(([, value]) => value.trim() !== ""),
            )
          : undefined;
        const result = await api.oauthFlows.requestToken(
          provider.key,
          filteredParams,
        );
        if (result.user_code) setAuthUserCode(result.user_code);
        if (result.verification_uri) setAuthVerificationUri(result.verification_uri);
        if (result.url) {
          setAuthUrl(result.url);
          openedUrlRef.current = result.url;
          window.open(result.url, "_blank", "noopener,noreferrer");
        }
        if (result.prompt && result.prompt_id) {
          setInputPrompt(result.prompt);
          setInputPromptId(result.prompt_id);
          setPhase("input");
        } else if (result.user_code || result.verification_uri) {
          setPhase("challenge");
        } else {
          setPhase("waiting");
        }
        const state = result.state || "";
        setAuthState(state);
        if (state) startPolling(provider.display_name || provider.key, state);
      } catch (err) {
        closeDialog();
        toast.error(
          `Failed to start auth flow for ${provider.display_name || provider.key}`,
          { description: err instanceof Error ? err.message : undefined },
        );
      }
    },
    [closeDialog, resetDialog, startPolling],
  );

  const handleSubmitInput = useCallback(async () => {
    if (!authState || !inputPromptId || inputValue.trim() === "") return;
    setSubmittingInput(true);
    try {
      await api.oauthFlows.submitAuthInput({
        state: authState,
        prompt_id: inputPromptId,
        input: inputValue.trim(),
      });
      setInputPrompt("");
      setInputPromptId("");
      setInputValue("");
      setPhase("waiting");
    } catch (err) {
      toast.error("Failed to submit the provider prompt", {
        description: err instanceof Error ? err.message : undefined,
      });
    } finally {
      setSubmittingInput(false);
    }
  }, [authState, inputPromptId, inputValue]);

  const handleCancel = useCallback(async () => {
    if (!authState) {
      closeDialog();
      return;
    }
    try {
      const result = await api.oauthFlows.cancelAuthSession(authState);
      if (result.cancelled) {
        closeDialog();
        toast.info("Authentication cancelled");
      } else {
        toast.info("The provider is finishing this login; it cannot be cancelled");
      }
    } catch (err) {
      // A 409 means the credential is being committed; keep polling for it.
      toast.info(
        "The provider is finishing this login; it cannot be cancelled",
        { description: err instanceof Error ? err.message : undefined },
      );
    }
  }, [authState, closeDialog]);

  const handleLogin = useCallback(
    (provider: OAuthProvider) => {
      if (provider.available === false) {
        toast.error(
          provider.unavailable_reason ||
            `${provider.display_name || provider.key} is unavailable`,
        );
        return;
      }
      const params = PROVIDER_PARAMS[provider.key];
      if (params?.some((param) => param.required)) {
        setParamDialogProvider(provider);
        setParamDialogValues({});
        setParamDialogOpen(true);
        return;
      }
      void startAuthFlow(provider);
    },
    [startAuthFlow],
  );

  const handleParamSubmit = useCallback(() => {
    if (!paramDialogProvider) return;
    const params = PROVIDER_PARAMS[paramDialogProvider.key];
    const missing = params?.filter(
      (param) => param.required && !paramDialogValues[param.key]?.trim(),
    );
    if (missing && missing.length > 0) {
      toast.error(`Required field: ${missing.map((p) => p.label).join(", ")}`);
      return;
    }
    setParamDialogOpen(false);
    void startAuthFlow(paramDialogProvider, paramDialogValues);
  }, [paramDialogProvider, paramDialogValues, startAuthFlow]);

  const handleDialogClose = useCallback(
    (open: boolean) => {
      if (!open) void handleCancel();
    },
    [handleCancel],
  );

  const handleCopyCode = useCallback((code: string) => {
    navigator.clipboard.writeText(code).then(
      () => toast.success("Code copied to clipboard"),
      () => toast.error("Failed to copy code"),
    );
  }, []);

  useEffect(() => {
    return () => stopPolling();
  }, [stopPolling]);

  if (loading) {
    return (
      <div className="grid grid-cols-1 gap-4 md:grid-cols-2 lg:grid-cols-3">
        {Array.from({ length: 6 }).map((_, index) => (
          <Card key={index}>
            <CardHeader>
              <Skeleton className="h-5 w-28" />
              <Skeleton className="h-4 w-20" />
            </CardHeader>
            <CardContent>
              <Skeleton className="h-4 w-24" />
            </CardContent>
          </Card>
        ))}
      </div>
    );
  }

  if (providers.length === 0) {
    return (
      <div className="flex flex-col items-center justify-center gap-2 rounded-lg border border-dashed p-12 text-center">
        <KeyRound className="size-10 text-muted-foreground/50" />
        <p className="text-sm text-muted-foreground">
          No OAuth providers available.
        </p>
      </div>
    );
  }

  return (
    <>
      <div className="grid grid-cols-1 gap-4 md:grid-cols-2 lg:grid-cols-3">
        {providers.map((provider) => {
          const hasAuth = providerHasAuth(provider);
          const unavailable = provider.available === false;
          return (
            <Card key={provider.key} className={unavailable ? "opacity-60" : ""}>
              <CardHeader>
                <div className="flex items-center justify-between">
                  <CardTitle className="text-base">
                    {provider.display_name || provider.key}
                  </CardTitle>
                  <div className="flex items-center gap-1.5">
                    {unavailable && (
                      <Badge variant="outline" className="text-xs">
                        Unavailable
                      </Badge>
                    )}
                    {flowTypeBadge(provider.flow_type || provider.flow || "oauth")}
                  </div>
                </div>
                <CardDescription className="text-xs text-muted-foreground">
                  {provider.key}
                </CardDescription>
              </CardHeader>
              <CardContent>
                <div className="flex flex-col gap-2">
                  <div className="flex items-center justify-between">
                    {authStatusBadge(hasAuth && !unavailable)}
                    <Button
                      size="sm"
                      variant={hasAuth ? "outline" : "default"}
                      onClick={() => handleLogin(provider)}
                      disabled={
                        unavailable ||
                        (authDialogOpen && authDialogProvider === provider.key)
                      }
                      title={unavailable ? provider.unavailable_reason : undefined}
                    >
                      <ExternalLink />
                      {hasAuth ? "Add Account" : "Login"}
                    </Button>
                  </div>
                  {unavailable && provider.unavailable_reason && (
                    <p className="text-xs text-muted-foreground">
                      {provider.unavailable_reason}
                    </p>
                  )}
                </div>
              </CardContent>
            </Card>
          );
        })}
      </div>

      <Dialog open={authDialogOpen} onOpenChange={handleDialogClose}>
        <DialogContent
          className="sm:max-w-md"
          onInteractOutside={(event) => event.preventDefault()}
        >
          <DialogHeader>
            <DialogTitle>
              Authenticating {authDialogDisplayName}
            </DialogTitle>
            <DialogDescription>
              {phase === "challenge"
                ? "Visit the provider page and complete the login."
                : phase === "input"
                  ? "The provider needs one more value from you."
                  : "Waiting for authentication to complete..."}
            </DialogDescription>
          </DialogHeader>
          <div className="flex flex-col items-center gap-4 py-4">
            {phase !== "input" && <Spinner className="size-8" />}
            {(authUrl || authVerificationUri) && (
              <div className="flex flex-col gap-1 text-center">
                <span className="text-sm text-muted-foreground">
                  Verification URL
                </span>
                <a
                  href={authVerificationUri || authUrl}
                  target="_blank"
                  rel="noopener noreferrer"
                  className="break-all text-sm font-medium text-primary underline underline-offset-4"
                >
                  {authVerificationUri || authUrl}
                </a>
              </div>
            )}
            {authUserCode && (
              <div className="flex flex-col gap-1 text-center">
                <span className="text-sm text-muted-foreground">Code</span>
                <div className="flex items-center gap-2">
                  <code className="rounded-md bg-muted px-3 py-1.5 text-lg font-mono font-semibold tracking-wider">
                    {authUserCode}
                  </code>
                  <Button
                    variant="outline"
                    size="icon-xs"
                    onClick={() => handleCopyCode(authUserCode)}
                  >
                    <Copy className="size-3" />
                  </Button>
                </div>
              </div>
            )}
            {phase === "input" && (
              <div className="flex w-full flex-col gap-2">
                <p className="text-sm text-muted-foreground">{inputPrompt}</p>
                <Input
                  value={inputValue}
                  placeholder="Paste the code or redirect URL here"
                  onChange={(event) => setInputValue(event.target.value)}
                  onKeyDown={(event) => {
                    if (event.key === "Enter") void handleSubmitInput();
                  }}
                />
                <Button
                  onClick={() => void handleSubmitInput()}
                  disabled={submittingInput || inputValue.trim() === ""}
                >
                  {submittingInput ? "Submitting..." : "Continue"}
                </Button>
              </div>
            )}
            {!authUrl &&
              !authVerificationUri &&
              !authUserCode &&
              phase === "waiting" && (
                <p className="text-sm text-muted-foreground">
                  Waiting for the backend to return the next step...
                </p>
              )}
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => void handleCancel()}>
              Cancel
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={paramDialogOpen} onOpenChange={setParamDialogOpen}>
        <DialogContent className="sm:max-w-md">
          <DialogHeader>
            <DialogTitle>
              Login to {paramDialogProvider?.display_name}
            </DialogTitle>
            <DialogDescription>
              This provider requires additional parameters to authenticate.
            </DialogDescription>
          </DialogHeader>
          <div className="flex flex-col gap-4">
            {paramDialogProvider &&
              PROVIDER_PARAMS[paramDialogProvider.key]?.map((param) => (
                <div key={param.key} className="flex flex-col gap-2">
                  <Label htmlFor={`param-${param.key}`}>
                    {param.label}
                    {param.required && (
                      <span className="text-destructive ml-1">*</span>
                    )}
                  </Label>
                  <Input
                    id={`param-${param.key}`}
                    type={param.secret ? "password" : "text"}
                    placeholder={param.placeholder}
                    value={paramDialogValues[param.key] ?? ""}
                    onChange={(event) =>
                      setParamDialogValues((prev) => ({
                        ...prev,
                        [param.key]: event.target.value,
                      }))
                    }
                  />
                </div>
              ))}
          </div>
          <DialogFooter>
            <Button
              variant="outline"
              onClick={() => setParamDialogOpen(false)}
            >
              Cancel
            </Button>
            <Button onClick={handleParamSubmit}>Continue</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  );
}
