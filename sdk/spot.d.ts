// Type definitions for the Spot browser SDK, loaded via
// <script src="/spot.js"></script>. Served at /spot.d.ts; reference it from
// TypeScript with:  /// <reference path="/spot.d.ts" />

declare namespace SpotSDK {
  /** Coarse, machine-readable classification of a failed Spot call. */
  type ErrorCode =
    | 'rate_limited'
    | 'forbidden'
    | 'unauthorized'
    | 'not_found'
    | 'conflict'
    | 'bad_request'
    | 'server'
    | 'network'
    | 'stream'
    | 'error';

  /** Error thrown by every Spot call. */
  class SpotError extends Error {
    status: number;
    code: ErrorCode;
    /** Seconds to wait; present on a 429. */
    retryAfter?: number;
  }

  /** Retry control: true (smart auto-retry, the default), false, or a max-retry count. */
  type RetryOption = boolean | number;

  interface Config {
    retry?: RetryOption;
    maxRetries?: number;
    retryBaseMs?: number;
    retryCapMs?: number;
  }

  interface Identity {
    email: string;
    name: string;
    peer_name: string;
    peer_ip: string;
    groups: string[];
    /** Whether this visitor may call spot.ai on this site. */
    ai_allowed: boolean;
  }

  interface Document<T = Record<string, unknown>> {
    id: string;
    owner: string;
    data: T;
    created_at: string;
    updated_at: string;
  }

  type FilterOp = 'eq' | 'ne' | 'gt' | 'gte' | 'lt' | 'lte' | 'in';
  /** field -> value (equality) or field -> { op: value }. */
  type Where = Record<string, unknown | Partial<Record<FilterOp, unknown>>>;

  interface ListOptions {
    limit?: number;
    mine?: boolean;
    after?: string;
    where?: Where;
    sort?: string;
    order?: 'asc' | 'desc';
  }

  interface SubscribeHandlers<T = Record<string, unknown>> {
    onCreate?: (doc: Document<T>) => void;
    onUpdate?: (doc: Document<T>) => void;
    onDelete?: (id: string) => void;
    onError?: (err: Error) => void;
  }

  interface Collection<T = Record<string, unknown>> {
    list(opts?: ListOptions): Promise<Document<T>[]>;
    iterate(opts?: { pageSize?: number; mine?: boolean; where?: Where }): AsyncGenerator<Document<T>>;
    count(opts?: { where?: Where; mine?: boolean }): Promise<number>;
    getMany(ids: string[]): Promise<Document<T>[]>;
    create(data: T): Promise<Document<T>>;
    get(id: string): Promise<Document<T>>;
    update(id: string, data: T, opts?: { mine?: boolean }): Promise<Document<T>>;
    updateMine(id: string, data: T): Promise<Document<T>>;
    delete(id: string, opts?: { mine?: boolean }): Promise<null>;
    deleteMine(id: string): Promise<null>;
    increment(id: string, field: string, by?: number, opts?: { mine?: boolean }): Promise<Document<T>>;
    incrementMine(id: string, field: string, by?: number): Promise<Document<T>>;
    subscribe(handlers: SubscribeHandlers<T>, opts?: { replay?: boolean }): () => void;
  }

  interface ChatMessage {
    role: 'user' | 'assistant' | 'system';
    content: string;
  }
  interface ChatResult {
    text: string;
    model: string;
    stop_reason: string;
    usage: unknown;
  }
  interface ChatOptions {
    model?: string;
    system?: string;
    max_tokens?: number;
  }
  interface StreamOptions extends ChatOptions {
    onToken?: (delta: string, text: string) => void;
    signal?: AbortSignal;
  }
  interface ImageResult {
    provider: string;
    model: string;
    images: Array<{ b64: string; mime_type: string; data_url: string }>;
  }

  interface StoredFile {
    id: string;
    name: string;
    size: number;
    content_type?: string;
    url: string;
  }

  interface SiteInfo {
    name: string;
    [key: string]: unknown;
  }

  interface RoomMessage<D = unknown> {
    event: string;
    room: string;
    from: { email?: string; [key: string]: unknown };
    data: D;
    sent_at: string;
  }
  type RoomStatus = 'connecting' | 'open' | 'reconnecting' | 'closed';
  interface Room {
    send(event: string, data?: unknown): void;
    setPresence(data?: unknown): void;
    on(event: string, handler: (msg: RoomMessage) => void): () => void;
    onPresence(handler: (users: unknown[]) => void): () => void;
    onError(handler: (err: Error) => void): () => void;
    onStatus(handler: (status: RoomStatus) => void): () => void;
    close(): void;
  }

  interface Spot {
    SpotError: typeof SpotError;
    /** Set default request behavior; returns the resolved config. */
    configure(opts?: Config): Config;
    me(): Promise<Identity>;
    db: { collection<T = Record<string, unknown>>(name: string): Collection<T> };
    realtime: { room(name: string): Room };
    ai: {
      chat(messages: ChatMessage[], opts?: ChatOptions): Promise<ChatResult>;
      stream(messages: ChatMessage[], opts?: StreamOptions): Promise<ChatResult>;
      image(prompt: string, opts?: Record<string, unknown>): Promise<ImageResult>;
    };
    files: {
      upload(file: File | Blob, opts?: { name?: string }): Promise<StoredFile>;
      list(): Promise<StoredFile[]>;
      delete(file: StoredFile | string, name?: string): Promise<null>;
    };
    sites: {
      mine(): Promise<SiteInfo[]>;
      public(): Promise<SiteInfo[]>;
      delete(name: string): Promise<null>;
    };
  }
}

declare const spot: SpotSDK.Spot;

interface Window {
  spot: SpotSDK.Spot;
}
