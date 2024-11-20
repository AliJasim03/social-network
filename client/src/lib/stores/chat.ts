import { writable } from 'svelte/store';
import type { Message, User } from '$lib/types';

interface ChatState {
    messages: Message[];
    activeChat: number | null; // userId of active chat
    contacts: User[];
    socket: WebSocket | null;
}

interface SendMessageOptions {
    url?: string;
    fileName?: string;
    fileType?: string;
}

function createChatStore() {
    const { subscribe, set, update } = writable<ChatState>({
        messages: [],
        activeChat: null,
        contacts: [],
        socket: null
    });

    let socket: WebSocket | null = null;

    return {
        subscribe,
        initialize: () => {
            socket = new WebSocket('ws://localhost:8080/ws');
            
            socket.onmessage = (event) => {
                const message = JSON.parse(event.data);
                if (message.type === 'chat') {
                    update(state => ({
                        ...state,
                        messages: [...state.messages, message]
                    }));
                }
            };

            update(state => ({ ...state, socket }));
        },
        loadMessages: async (userId: number) => {
            try {
                const response = await fetch(`http://localhost:8080/messages/${userId}`, {
                    credentials: 'include'
                });
                if (response.ok) {
                    const messages = await response.json();
                    update(state => ({ ...state, messages, activeChat: userId }));
                }
            } catch (error) {
                console.error('Failed to load messages:', error);
            }
        },
        sendMessage: (content: string, recipientId: number | null, fileOptions?: SendMessageOptions) => {
            if (!socket || recipientId === null) return;

            const message = {
                type: 'chat',
                content,
                recipientId,
                fileUrl: fileOptions?.url,
                fileName: fileOptions?.fileName,
                fileType: fileOptions?.fileType
            };

            socket.send(JSON.stringify(message));
        },
        loadContacts: async () => {
            try {
                const response = await fetch('http://localhost:8080/contacts', {
                    credentials: 'include'
                });
                if (response.ok) {
                    const contacts = await response.json();
                    update(state => ({ ...state, contacts }));
                }
            } catch (error) {
                console.error('Failed to load contacts:', error);
            }
        },
        cleanup: () => {
            if (socket) {
                socket.close();
                socket = null;
            }
            set({
                messages: [],
                activeChat: null,
                contacts: [],
                socket: null
            });
        }
    };
}

export const chat = createChatStore(); 