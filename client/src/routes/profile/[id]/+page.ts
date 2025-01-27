import type {PageLoad} from './$types';
import type {User} from "$lib/types";
import {transformUser} from '$lib/utils/transformer'
import {error} from "@sveltejs/kit";

export const load: PageLoad = async ({params, fetch}) => {
    const response = await fetch(`http://localhost:8080/user/${params.id}`, {
        credentials: 'include'
    });
    if (response.ok) {
        const userData = await response.json();
        const user: User = transformUser(userData);
        return {
            user,
            params
        };
    }
    throw error(404, 'User not found');

};
